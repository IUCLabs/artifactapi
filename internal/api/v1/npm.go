package v1

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/storage"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// Local npm repos speak the npm registry publish/read protocol. Per package:
//   <pkg>/-/<base>-<version>.tgz   tarball (immutable)
//   <pkg>/-/<base>-<version>.json  version manifest (immutable)
//   <pkg>/-/dist-tags/<tag>        version string (mutable)

var (
	npmNameRe   = regexp.MustCompile(`^(@[a-z0-9][a-z0-9._~-]*/)?[a-z0-9][a-z0-9._~-]*$`)
	npmRefRe    = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]*$`)
	npmSemverRe = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)

	npmMaxPublishBytes int64 = 256 << 20
)

func (h *ProxyHandler) localNPMRemote(r *http.Request, name string) (*models.Remote, bool) {
	remote, err := h.db.GetRemote(r.Context(), name)
	if err != nil {
		return nil, false
	}
	return remote, remote.RepoType == models.RepoTypeLocal && remote.PackageType == models.PackageNPM
}

// npmPackageName returns the package a packument/publish path addresses, or ""
// when the path is something else (e.g. a tarball under <pkg>/-/).
func npmPackageName(raw string) string {
	p, err := url.PathUnescape(raw)
	if err != nil {
		return ""
	}
	p = strings.TrimSuffix(p, "/")
	if !npmNameRe.MatchString(p) {
		return ""
	}
	return p
}

type npmRelease struct {
	version  string
	manifest []byte
	tarball  []byte
	tags     []string
}

func parseNPMPublish(body io.Reader, pkg string) (*npmRelease, error) {
	var doc struct {
		Name        string                     `json:"name"`
		DistTags    map[string]string          `json:"dist-tags"`
		Versions    map[string]json.RawMessage `json:"versions"`
		Attachments map[string]struct {
			Data string `json:"data"`
		} `json:"_attachments"`
	}
	if err := json.NewDecoder(body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("invalid publish body: %w", err)
	}
	if doc.Name != pkg {
		return nil, fmt.Errorf("body name %q does not match %q", doc.Name, pkg)
	}
	if len(doc.Versions) != 1 || len(doc.Attachments) != 1 {
		return nil, errors.New("publish must carry exactly one version and one tarball")
	}

	rel := &npmRelease{}
	for v, m := range doc.Versions {
		rel.version, rel.manifest = v, m
	}
	if !npmSemverRe.MatchString(rel.version) {
		return nil, fmt.Errorf("invalid version %q", rel.version)
	}
	var meta struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Dist    struct {
			Integrity string `json:"integrity"`
			Shasum    string `json:"shasum"`
		} `json:"dist"`
	}
	if err := json.Unmarshal(rel.manifest, &meta); err != nil {
		return nil, fmt.Errorf("invalid version manifest: %w", err)
	}
	if meta.Name != pkg || meta.Version != rel.version {
		return nil, errors.New("version manifest name/version mismatch")
	}

	for _, a := range doc.Attachments {
		tgz, err := base64.StdEncoding.DecodeString(a.Data)
		if err != nil {
			return nil, fmt.Errorf("invalid tarball encoding: %w", err)
		}
		rel.tarball = tgz
	}
	if err := verifyNPMDist(rel.tarball, meta.Dist.Integrity, meta.Dist.Shasum); err != nil {
		return nil, err
	}

	for tag, v := range doc.DistTags {
		if !npmRefRe.MatchString(tag) || v != rel.version {
			return nil, fmt.Errorf("dist-tag %q must point at the published version", tag)
		}
		rel.tags = append(rel.tags, tag)
	}
	return rel, nil
}

func verifyNPMDist(tgz []byte, integrity, shasum string) error {
	if b64, ok := strings.CutPrefix(integrity, "sha512-"); ok {
		sum := sha512.Sum512(tgz)
		if base64.StdEncoding.EncodeToString(sum[:]) != b64 {
			return errors.New("tarball does not match dist.integrity")
		}
		return nil
	}
	if shasum != "" {
		sum := sha1.Sum(tgz)
		if hex.EncodeToString(sum[:]) != shasum {
			return errors.New("tarball does not match dist.shasum")
		}
		return nil
	}
	return errors.New("version manifest carries no sha512 integrity or shasum")
}

func npmError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (h *ProxyHandler) handleLocalPut(w http.ResponseWriter, r *http.Request) {
	remote, ok := h.localNPMRemote(r, chi.URLParam(r, "localName"))
	if !ok {
		npmError(w, http.StatusMethodNotAllowed, "publish is only supported on local npm repositories")
		return
	}
	pkg := npmPackageName(chi.URLParam(r, "*"))
	if pkg == "" {
		npmError(w, http.StatusBadRequest, "invalid package name")
		return
	}
	rel, err := parseNPMPublish(http.MaxBytesReader(w, r.Body, npmMaxPublishBytes), pkg)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			npmError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("publish body exceeds %d bytes", npmMaxPublishBytes))
			return
		}
		npmError(w, http.StatusBadRequest, err.Error())
		return
	}

	base := pkg[strings.LastIndex(pkg, "/")+1:]
	stem := pkg + "/-/" + base + "-" + rel.version
	// The manifest gates visibility and tags may only name visible versions, so
	// the order is tarball, manifest, tags. A tarball left by a failed publish is
	// accepted again only with identical bytes; an existing manifest always conflicts.
	conflict := func() {
		npmError(w, http.StatusConflict, fmt.Sprintf("%s@%s already exists; overwrites are not allowed", pkg, rel.version))
	}
	ok, err = h.npmStoreImmutable(r.Context(), remote.Name, stem+".tgz", rel.tarball, "application/octet-stream")
	if err != nil {
		npmError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		conflict()
		return
	}
	if _, err := h.npmStore(r.Context(), remote.Name, stem+".json", rel.manifest, "application/json", false); err != nil {
		if errors.Is(err, database.ErrAlreadyExists) {
			conflict()
			return
		}
		npmError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, tag := range rel.tags {
		if _, err := h.npmStore(r.Context(), remote.Name, pkg+"/-/dist-tags/"+tag, []byte(rel.version), "text/plain", true); err != nil {
			npmError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	slog.Info("local npm package published", "repo", remote.Name, "package", pkg, "version", rel.version)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": pkg})
}

// npmStoreImmutable reports false when path already holds different bytes.
func (h *ProxyHandler) npmStoreImmutable(ctx context.Context, repo, path string, data []byte, contentType string) (bool, error) {
	hash, err := h.npmStore(ctx, repo, path, data, contentType, false)
	if !errors.Is(err, database.ErrAlreadyExists) {
		return err == nil, err
	}
	existing, err := h.db.GetLocalFile(ctx, repo, path)
	if err != nil || existing == nil {
		return false, fmt.Errorf("read existing %s: %v", path, err)
	}
	return existing.ContentHash == hash, nil
}

func (h *ProxyHandler) npmStore(ctx context.Context, repo, path string, data []byte, contentType string, mutable bool) (string, error) {
	result, err := h.cas.Store(ctx, bytes.NewReader(data), contentType)
	if err != nil {
		return "", err
	}
	if err := h.db.UpsertBlob(ctx, result.ContentHash, result.S3Key, result.SizeBytes, contentType); err != nil {
		return "", err
	}
	if mutable {
		return result.ContentHash, h.db.UpsertLocalFile(ctx, repo, path, result.ContentHash)
	}
	return result.ContentHash, h.db.CreateLocalFile(ctx, repo, path, result.ContentHash)
}

func (h *ProxyHandler) readLocalBlob(ctx context.Context, contentHash string) ([]byte, error) {
	reader, _, err := h.store.Download(ctx, storage.BlobKey(strings.TrimPrefix(contentHash, "sha256:")))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

func (h *ProxyHandler) serveNPMPackument(w http.ResponseWriter, r *http.Request, remote *models.Remote, pkg string) {
	files, err := h.db.ListLocalFilesByPrefix(r.Context(), remote.Name, pkg+"/-/")
	if err != nil {
		npmError(w, http.StatusInternalServerError, err.Error())
		return
	}
	read := func(hash string) ([]byte, error) { return h.readLocalBlob(r.Context(), hash) }
	tarballBase := fmt.Sprintf("%s://%s/api/v1/local/%s/", scheme(r), r.Host, remote.Name)
	doc, err := buildNPMPackument(pkg, files, read, tarballBase)
	if err != nil {
		npmError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if doc == nil {
		npmError(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Artifact-Source", "local")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(doc)
}

// buildNPMPackument returns nil when pkg has no published versions.
func buildNPMPackument(pkg string, files []database.LocalFile, read func(hash string) ([]byte, error), tarballBase string) (map[string]any, error) {
	prefix := pkg + "/-/"
	versions := map[string]any{}
	tags := map[string]string{}
	for _, f := range files {
		// LIKE treats '_' as a wildcard, so the prefix match can over-select.
		rest, ok := strings.CutPrefix(f.FilePath, prefix)
		if !ok {
			continue
		}
		if tag, isTag := strings.CutPrefix(rest, "dist-tags/"); isTag {
			v, err := read(f.ContentHash)
			if err != nil {
				return nil, err
			}
			tags[tag] = string(v)
			continue
		}
		stem, isManifest := strings.CutSuffix(rest, ".json")
		if !isManifest {
			continue
		}
		raw, err := read(f.ContentHash)
		if err != nil {
			return nil, err
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("manifest %s: %w", f.FilePath, err)
		}
		dist, _ := m["dist"].(map[string]any)
		if dist == nil {
			dist = map[string]any{}
			m["dist"] = dist
		}
		dist["tarball"] = tarballBase + prefix + stem + ".tgz"
		v, _ := m["version"].(string)
		versions[v] = m
	}
	if len(versions) == 0 {
		return nil, nil
	}
	for tag, v := range tags {
		if _, ok := versions[v]; !ok {
			delete(tags, tag)
		}
	}
	return map[string]any{"_id": pkg, "name": pkg, "dist-tags": tags, "versions": versions}, nil
}
