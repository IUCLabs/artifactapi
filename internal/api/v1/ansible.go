package v1

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/IUCLabs/artifactapi/internal/api/requrl"
	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/provider/ansible"
	"github.com/IUCLabs/artifactapi/internal/virtual"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

var ansibleMaxPublishBytes int64 = 256 << 20

// ansibleError writes a Galaxy v3 error document, which ansible-galaxy prints.
func ansibleError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{
		"status": strconv.Itoa(status), "code": strings.ToLower(strings.ReplaceAll(http.StatusText(status), " ", "_")),
		"title": http.StatusText(status), "detail": msg,
	}}})
}

func writeAnsibleJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "Host, X-Forwarded-Proto")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func isAnsiblePublish(path string) bool {
	return strings.TrimSuffix(path, "/")+"/" == ansible.PublishPath
}

func (h *ProxyHandler) handleAnsiblePublish(w http.ResponseWriter, r *http.Request) {
	remote, err := h.db.GetRemote(r.Context(), chi.URLParam(r, "localName"))
	if err != nil || remote.RepoType != models.RepoTypeLocal || remote.PackageType != models.PackageAnsible {
		ansibleError(w, http.StatusMethodNotAllowed, "publish is only supported on local ansible repositories")
		return
	}
	base, ok := requrl.BaseURL(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, ansibleMaxPublishBytes)
	file, sum, err := readAnsibleUpload(r)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			ansibleError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("upload exceeds %d bytes", ansibleMaxPublishBytes))
			return
		}
		ansibleError(w, http.StatusBadRequest, err.Error())
		return
	}
	digest := sha256.Sum256(file)
	if got := hex.EncodeToString(digest[:]); sum != "" && !strings.EqualFold(sum, got) {
		ansibleError(w, http.StatusBadRequest, "sha256 does not match the uploaded file")
		return
	}
	e, err := ansible.ParseCollection(bytes.NewReader(file))
	if err != nil {
		ansibleError(w, http.StatusBadRequest, err.Error())
		return
	}
	e.SHA256, e.Size, e.CreatedAt = hex.EncodeToString(digest[:]), int64(len(file)), time.Now().UTC()

	id := e.Namespace + "." + e.Name + ":" + e.Version
	unlock, err := h.engine.Lock(r.Context(), remote.Name, "_ansiblelock/"+e.TaskID(), gemsLockWait, 5*time.Minute)
	if err != nil {
		ansibleError(w, http.StatusServiceUnavailable, id+" is being published, retry")
		return
	}
	defer unlock()
	conflict := func() {
		ansibleError(w, http.StatusConflict, id+" already exists; overwrites are not allowed")
	}
	if f, err := h.db.GetLocalFile(r.Context(), remote.Name, e.IndexPath()); err != nil {
		ansibleError(w, http.StatusInternalServerError, err.Error())
		return
	} else if f != nil {
		conflict()
		return
	}
	// The version is not live, so a tarball left by a failed publish is safe to drop.
	artifact := ansible.ArtifactPrefix + e.Filename()
	fail := func(status int, msg string) {
		if err := h.db.DeleteLocalFile(context.WithoutCancel(r.Context()), remote.Name, artifact); err != nil {
			slog.Error("ansible publish cleanup failed", "repo", remote.Name, "path", artifact, "error", err)
		}
		ansibleError(w, status, msg)
	}
	if ok, err := h.npmStoreImmutable(r.Context(), remote.Name, artifact, file, "application/gzip"); err != nil {
		fail(http.StatusInternalServerError, err.Error())
		return
	} else if !ok {
		fail(http.StatusConflict, id+" already exists; overwrites are not allowed")
		return
	}
	entry, err := json.Marshal(e)
	if err != nil {
		fail(http.StatusInternalServerError, err.Error())
		return
	}
	// The index entry gates visibility, so it is written last.
	if _, err := h.npmStore(r.Context(), remote.Name, e.IndexPath(), entry, "application/json", false); err != nil {
		if errors.Is(err, database.ErrAlreadyExists) {
			conflict()
			return
		}
		fail(http.StatusInternalServerError, err.Error())
		return
	}
	slog.Info("local ansible collection published", "repo", remote.Name, "collection", id)
	repoBase := base + "/api/v1/local/" + url.PathEscape(remote.Name)
	writeAnsibleJSON(w, http.StatusAccepted, map[string]string{"task": repoBase + "/api/v3/imports/collections/" + e.TaskID() + "/"})
}

// readAnsibleUpload returns the multipart "file" part, which ansible-galaxy
// base64-encodes, and the optional "sha256" field sent with it.
func readAnsibleUpload(r *http.Request) ([]byte, string, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, "", errors.New("expected a multipart/form-data upload")
	}
	var file []byte
	var sum string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", err
		}
		switch part.FormName() {
		case "file":
			if file != nil {
				return nil, "", errors.New("multiple file parts")
			}
			var src io.Reader = part
			switch enc := strings.ToLower(part.Header.Get("Content-Transfer-Encoding")); enc {
			case "base64":
				src = base64.NewDecoder(base64.StdEncoding, part)
			case "", "binary", "7bit", "8bit":
			default:
				return nil, "", fmt.Errorf("unsupported Content-Transfer-Encoding %q", enc)
			}
			if file, err = io.ReadAll(src); err != nil {
				return nil, "", err
			}
		case "sha256":
			b, err := io.ReadAll(io.LimitReader(part, 128))
			if err != nil {
				return nil, "", err
			}
			sum = strings.TrimSpace(string(b))
		}
	}
	if file == nil {
		return nil, "", errors.New("missing file part")
	}
	return file, sum, nil
}

// serveLocalAnsible answers the Galaxy v3 API of a local ansible repo.
func (h *ProxyHandler) serveLocalAnsible(w http.ResponseWriter, r *http.Request, remote *models.Remote, ap ansible.APIPath) {
	base, ok := requrl.BaseURL(w, r)
	if !ok {
		return
	}
	body, err := h.localAnsibleDoc(r.Context(), remote.Name, ap, r.URL.Query(), base+"/api/v1/local/"+url.PathEscape(remote.Name))
	if errors.Is(err, virtual.ErrNotFound) {
		ansibleError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		ansibleError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("X-Artifact-Source", "local")
	writeAnsibleJSON(w, http.StatusOK, body)
}

// LocalAnsible serves a local ansible repo's collection, full versions list
// or version detail to ansible virtuals, with root-relative URLs.
func (h *ProxyHandler) LocalAnsible(ctx context.Context, repo, path string) ([]byte, error) {
	doc, err := h.localAnsibleDoc(ctx, repo, ansible.ParseAPIPath(path), nil, "/api/v1/local/"+url.PathEscape(repo))
	if err != nil {
		return nil, err
	}
	return json.Marshal(doc)
}

// localAnsibleDoc builds the document for ap; a nil q lists every version.
func (h *ProxyHandler) localAnsibleDoc(ctx context.Context, repo string, ap ansible.APIPath, q url.Values, base string) (any, error) {
	switch ap.Kind {
	case ansible.APIRoot:
		return ansible.Root(), nil
	case ansible.APIImport:
		ns, rest, _ := strings.Cut(ap.TaskID, "-")
		name, version, _ := strings.Cut(rest, "-")
		entries, err := h.localAnsibleEntries(ctx, repo, ns, name)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.Version == version {
				return ansible.ImportTask(e), nil
			}
		}
	case ansible.APICollection, ansible.APIVersions, ansible.APIVersion:
		entries, err := h.localAnsibleEntries(ctx, repo, ap.Namespace, ap.Name)
		if err != nil || len(entries) == 0 {
			return nil, cmp.Or(err, virtual.ErrNotFound)
		}
		refs := make([]ansible.VersionRef, len(entries))
		for i, e := range entries {
			refs[i] = e.Ref()
		}
		switch ap.Kind {
		case ansible.APICollection:
			return ansible.CollectionDoc(base, ap.Namespace, ap.Name, refs), nil
		case ansible.APIVersions:
			limit, offset := len(refs), 0
			if q != nil {
				limit, offset = ansible.Page(q)
			}
			return ansible.VersionsPage(base, ap.Namespace, ap.Name, refs, limit, offset), nil
		}
		for _, e := range entries {
			if e.Version == ap.Version {
				return ansible.VersionDoc(base, e), nil
			}
		}
	}
	return nil, virtual.ErrNotFound
}

// localAnsibleEntries returns a collection's versions, newest publish first.
func (h *ProxyHandler) localAnsibleEntries(ctx context.Context, repo, ns, name string) ([]ansible.Entry, error) {
	if !ansible.NameRe.MatchString(ns) || !ansible.NameRe.MatchString(name) {
		return nil, nil
	}
	prefix := ansible.IndexPrefix + ns + "/" + name + "/"
	files, err := h.db.ListLocalFilesByPrefix(ctx, repo, prefix)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(files, func(a, b database.LocalFile) int { return cmp.Compare(b.ID, a.ID) })
	var out []ansible.Entry
	for _, f := range files {
		// LIKE treats '_' as a wildcard, so the prefix match can over-select.
		if !strings.HasPrefix(f.FilePath, prefix) {
			continue
		}
		raw, err := h.readLocalBlob(ctx, f.ContentHash)
		if err != nil {
			return nil, err
		}
		var e ansible.Entry
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("decode %s: %w", f.FilePath, err)
		}
		out = append(out, e)
	}
	return out, nil
}
