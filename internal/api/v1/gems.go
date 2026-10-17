package v1

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/provider/gems"
	"github.com/IUCLabs/artifactapi/internal/virtual"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// Local gems repos speak the RubyGems push/yank API and compact index. Per gem:
//   gems/<full>.gem                         package (immutable)
//   quick/Marshal.4.8/<full>.gemspec.rz     marshalled spec (immutable)
//   _index/<name>/<full>                    compact index /info line; gates visibility
//   _yanked/<name>/<full>                   yank tombstone; blocks re-push of the version

var (
	gemsMaxPushBytes int64 = 256 << 20
	gemsLockWait           = 30 * time.Second
)

const (
	gemsIndexPrefix  = "_index/"
	gemsYankedPrefix = "_yanked/"
)

func gemsText(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg)
}

func (h *ProxyHandler) localGemsRemote(w http.ResponseWriter, r *http.Request) (*models.Remote, bool) {
	remote, err := h.db.GetRemote(r.Context(), chi.URLParam(r, "localName"))
	if err != nil || remote.RepoType != models.RepoTypeLocal || remote.PackageType != models.PackageGems {
		gemsText(w, http.StatusMethodNotAllowed, "only supported on local gems repositories")
		return nil, false
	}
	return remote, true
}

// lockGemVersion serialises push and yank of one gem version across replicas.
func (h *ProxyHandler) lockGemVersion(w http.ResponseWriter, r *http.Request, repo, full string) (func(), bool) {
	unlock, err := h.engine.Lock(r.Context(), repo, "_gemlock/"+full, gemsLockWait, 5*time.Minute)
	if err != nil {
		gemsText(w, http.StatusServiceUnavailable, full+" is being pushed or yanked, retry")
		return nil, false
	}
	return unlock, true
}

func (h *ProxyHandler) handleLocalPost(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "*") != "api/v1/gems" {
		gemsText(w, http.StatusNotFound, "not found")
		return
	}
	remote, ok := h.localGemsRemote(w, r)
	if !ok {
		return
	}
	gem, err := io.ReadAll(http.MaxBytesReader(w, r.Body, gemsMaxPushBytes))
	if err != nil {
		gemsText(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("gem exceeds %d bytes", gemsMaxPushBytes))
		return
	}
	spec, err := gems.ParseGem(gem)
	if err != nil {
		gemsText(w, http.StatusBadRequest, err.Error())
		return
	}
	quick, err := gems.QuickSpec(spec)
	if err != nil {
		gemsText(w, http.StatusInternalServerError, err.Error())
		return
	}

	full := spec.FullName()
	unlock, ok := h.lockGemVersion(w, r, remote.Name, full)
	if !ok {
		return
	}
	defer unlock()
	conflict := func() {
		gemsText(w, http.StatusConflict, fmt.Sprintf("%s already exists; overwrites are not allowed", full))
	}
	index := gemsIndexPrefix + spec.Name + "/" + full
	for _, p := range []string{gemsYankedPrefix + spec.Name + "/" + full, index} {
		f, err := h.db.GetLocalFile(r.Context(), remote.Name, p)
		if err != nil {
			gemsText(w, http.StatusInternalServerError, err.Error())
			return
		}
		if f != nil && p == index {
			conflict()
			return
		}
		if f != nil {
			gemsText(w, http.StatusConflict, fmt.Sprintf("%s was yanked; re-pushing a yanked version is not allowed", full))
			return
		}
	}
	// The version is not live, so on failure every path is an invisible leftover and safe to drop.
	paths := []string{"gems/" + full + ".gem", "quick/Marshal.4.8/" + full + ".gemspec.rz", index}
	fail := func(status int, msg string) {
		for _, p := range paths {
			if err := h.db.DeleteLocalFile(context.WithoutCancel(r.Context()), remote.Name, p); err != nil {
				slog.Error("gem push cleanup failed", "repo", remote.Name, "path", p, "error", err)
			}
		}
		gemsText(w, status, msg)
	}
	// The index line gates visibility, so it is written last.
	for _, f := range []struct {
		path string
		data []byte
	}{
		{paths[0], gem},
		{paths[1], quick},
	} {
		ok, err := h.npmStoreImmutable(r.Context(), remote.Name, f.path, f.data, "application/octet-stream")
		if err != nil {
			fail(http.StatusInternalServerError, err.Error())
			return
		}
		if !ok {
			fail(http.StatusConflict, fmt.Sprintf("%s already exists; overwrites are not allowed", full))
			return
		}
	}
	line := gems.InfoLine(spec, gem)
	if _, err := h.npmStore(r.Context(), remote.Name, index, []byte(line), "text/plain", false); err != nil {
		fail(http.StatusInternalServerError, err.Error())
		return
	}

	slog.Info("local gem pushed", "repo", remote.Name, "gem", full)
	gemsText(w, http.StatusOK, "Successfully registered gem: "+spec.Name+" ("+spec.Version+")")
}

func (h *ProxyHandler) handleLocalDelete(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "*") != "api/v1/gems/yank" {
		gemsText(w, http.StatusNotFound, "not found")
		return
	}
	remote, ok := h.localGemsRemote(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		gemsText(w, http.StatusRequestEntityTooLarge, "yank body too large")
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		gemsText(w, http.StatusBadRequest, "invalid form body")
		return
	}
	for k, v := range r.URL.Query() {
		if form.Get(k) == "" {
			form[k] = v
		}
	}
	spec := &gems.Spec{Name: form.Get("gem_name"), Version: form.Get("version"), Platform: form.Get("platform")}
	if spec.Platform == "" {
		spec.Platform = "ruby"
	}
	if !gems.NameRe.MatchString(spec.Name) || !gems.VersionRe.MatchString(spec.Version) || !gems.PlatformRe.MatchString(spec.Platform) {
		gemsText(w, http.StatusBadRequest, "invalid gem_name, version or platform")
		return
	}

	full := spec.FullName()
	unlock, ok := h.lockGemVersion(w, r, remote.Name, full)
	if !ok {
		return
	}
	defer unlock()
	index := gemsIndexPrefix + spec.Name + "/" + full
	existing, err := h.db.GetLocalFile(r.Context(), remote.Name, index)
	if err != nil {
		gemsText(w, http.StatusInternalServerError, err.Error())
		return
	}
	if existing == nil {
		gemsText(w, http.StatusNotFound, "The version "+spec.Version+" does not exist")
		return
	}
	// The tombstone keeps the yanked line and is written before any delete.
	line, err := h.readLocalBlob(r.Context(), existing.ContentHash)
	if err != nil {
		gemsText(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := h.npmStore(r.Context(), remote.Name, gemsYankedPrefix+spec.Name+"/"+full, line, "text/plain", false); err != nil && !errors.Is(err, database.ErrAlreadyExists) {
		gemsText(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, p := range []string{index, "gems/" + full + ".gem", "quick/Marshal.4.8/" + full + ".gemspec.rz"} {
		if err := h.db.DeleteLocalFile(context.WithoutCancel(r.Context()), remote.Name, p); err != nil {
			gemsText(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	slog.Info("local gem yanked", "repo", remote.Name, "gem", full)
	gemsText(w, http.StatusOK, "Successfully deleted gem: "+spec.Name+" ("+spec.Version+")")
}

// isGemsIndexPath reports whether path is a compact index document.
func isGemsIndexPath(path string) bool {
	return path == "versions" || path == "names" || strings.HasPrefix(path, "info/")
}

func (h *ProxyHandler) serveLocalGemsIndex(w http.ResponseWriter, r *http.Request, remote *models.Remote, path string) {
	body, err := h.LocalGemsIndex(r.Context(), remote.Name, path)
	if errors.Is(err, virtual.ErrNotFound) {
		gemsText(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		gemsText(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("X-Artifact-Source", "local")
	gemsText(w, http.StatusOK, string(body))
}

// LocalGemsIndex builds a local gems repo's versions, names or info/<gem>
// document, or returns virtual.ErrNotFound.
func (h *ProxyHandler) LocalGemsIndex(ctx context.Context, repo, path string) ([]byte, error) {
	if name, ok := strings.CutPrefix(path, "info/"); ok {
		if !gems.NameRe.MatchString(name) {
			return nil, virtual.ErrNotFound
		}
		lines, err := h.localGemsLines(ctx, repo, gemsIndexPrefix+name+"/")
		if err != nil {
			return nil, err
		}
		if len(lines[name]) == 0 {
			return nil, virtual.ErrNotFound
		}
		return gems.InfoBody(lines[name]), nil
	}
	if path != "versions" && path != "names" {
		return nil, virtual.ErrNotFound
	}
	lines, err := h.localGemsLines(ctx, repo, gemsIndexPrefix)
	if err != nil {
		return nil, err
	}
	names := slices.Sorted(maps.Keys(lines))
	var b strings.Builder
	if path == "names" {
		b.WriteString("---\n")
		for _, n := range names {
			b.WriteString(n + "\n")
		}
		return []byte(b.String()), nil
	}
	b.WriteString(gems.VersionsHeader(time.Now()))
	for _, n := range names {
		b.WriteString(gems.VersionsLine(n, gems.InfoBody(lines[n])) + "\n")
	}
	return []byte(b.String()), nil
}

// localGemsLines returns /info lines per gem name in push order.
func (h *ProxyHandler) localGemsLines(ctx context.Context, repo, prefix string) (map[string][]string, error) {
	files, err := h.db.ListLocalFilesByPrefix(ctx, repo, prefix)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(files, func(a, b database.LocalFile) int { return cmp.Compare(a.ID, b.ID) })
	out := map[string][]string{}
	for _, f := range files {
		// LIKE treats '_' as a wildcard, so the prefix match can over-select.
		rest, ok := strings.CutPrefix(f.FilePath, prefix)
		if !ok {
			continue
		}
		name := strings.TrimPrefix(prefix, gemsIndexPrefix)
		if name == "" {
			name, _, _ = strings.Cut(rest, "/")
		} else {
			name = strings.TrimSuffix(name, "/")
		}
		line, err := h.readLocalBlob(ctx, f.ContentHash)
		if err != nil {
			return nil, err
		}
		out[name] = append(out[name], string(line))
	}
	return out, nil
}
