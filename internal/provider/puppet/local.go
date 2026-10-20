package puppet

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/storage"
)

const filesPrefix = "v3/files/"

// releaseRe matches <owner>-<name>-<semver>, the Forge release slug and the
// tarball basename that `puppet module build` produces.
var releaseRe = regexp.MustCompile(`^([a-z0-9]+)-([a-z][a-z0-9_]*)-(\d+\.\d+\.\d+(?:-[0-9a-z.-]+)?(?:\+[0-9a-z.-]+)?)$`)

func ParseRelease(s string) (slug, version string, ok bool) {
	m := releaseRe.FindStringSubmatch(strings.ToLower(s))
	if m == nil {
		return "", "", false
	}
	return m[1] + "-" + m[2], m[3], true
}

var slugRe = regexp.MustCompile(`^[a-z0-9]+-[a-z][a-z0-9_]*$`)

// ValidSlug reports whether s is a normalized <owner>-<name> module slug.
func ValidSlug(s string) bool { return slugRe.MatchString(s) }

func NormalizeSlug(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "/", "-"))
}

// ValidateUpload stores tarballs under v3/files/ so Forge file_uri downloads
// resolve straight to the stored file.
func (p *Provider) ValidateUpload(filePath string) (storagePath, contentType string, err error) {
	filename := strings.ToLower(path.Base(path.Clean("/" + filePath)))
	if _, _, ok := ParseRelease(strings.TrimSuffix(filename, ".tar.gz")); !ok || !strings.HasSuffix(filename, ".tar.gz") {
		return "", "", fmt.Errorf("file must be named <owner>-<module>-<version>.tar.gz")
	}
	return filesPrefix + filename, "application/gzip", nil
}

func (p *Provider) UploadResponse(storagePath, contentHash string, sizeBytes int64) map[string]any {
	return map[string]any{
		"filename":     path.Base(storagePath),
		"content_hash": contentHash,
		"size_bytes":   sizeBytes,
	}
}

func (p *Provider) AfterUpload(ctx context.Context, repoName, storagePath, contentHash string, blobs provider.BlobReader, db provider.MetadataStore) {
	reader, size, err := blobs.Download(ctx, storage.BlobKey(strings.TrimPrefix(contentHash, "sha256:")))
	if err != nil {
		slog.Error("puppet metadata: download failed", "repo", repoName, "path", storagePath, "error", err)
		return
	}
	defer func() { _ = reader.Close() }()
	if size > maxModuleTarball {
		slog.Error("puppet metadata: tarball too large", "repo", repoName, "path", storagePath, "size", size)
		return
	}

	hash := md5.New()
	src := io.TeeReader(io.LimitReader(reader, maxModuleTarball), hash)
	meta, err := parseModule(src)
	if err != nil {
		slog.Error("puppet metadata: parse failed", "repo", repoName, "path", storagePath, "error", err)
		return
	}
	if _, err := io.Copy(io.Discard, src); err != nil {
		slog.Error("puppet metadata: read failed", "repo", repoName, "path", storagePath, "error", err)
		return
	}
	slug, version, _ := ParseRelease(strings.TrimSuffix(path.Base(storagePath), ".tar.gz"))
	if meta.Slug != slug || meta.Version != version {
		slog.Error("puppet metadata: metadata.json does not match filename", "repo", repoName, "path", storagePath, "name", meta.Slug, "version", meta.Version)
		return
	}
	meta.RepoName = repoName
	meta.FilePath = storagePath
	meta.ContentHash = contentHash
	meta.Size = size
	meta.MD5 = hex.EncodeToString(hash.Sum(nil))

	store, ok := db.(provider.PuppetMetadataStore)
	if !ok {
		slog.Error("puppet metadata: store does not support puppet metadata", "repo", repoName)
		return
	}
	if err := store.InsertPuppetMetadata(ctx, meta); err != nil {
		slog.Error("puppet metadata: insert failed", "repo", repoName, "path", storagePath, "error", err)
		return
	}
	slog.Info("puppet metadata: parsed", "repo", repoName, "module", meta.Slug, "version", meta.Version)
}

func (p *Provider) AfterDelete(ctx context.Context, repoName, storagePath string, db provider.MetadataDeleter) error {
	deleter, ok := db.(provider.PuppetMetadataDeleter)
	if !ok {
		return nil
	}
	return deleter.DeletePuppetMetadata(ctx, repoName, storagePath)
}

const maxMetadataJSON = 1 << 20

var (
	maxModuleTarball int64 = 100 << 20
	maxModuleScan    int64 = 200 << 20
)

// parseModule reads the top-level <dir>/metadata.json from a module tarball,
// scanning at most maxModuleScan decompressed bytes.
func parseModule(r io.Reader) (*provider.PuppetMetadata, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	scan := &io.LimitedReader{R: zr, N: maxModuleScan}
	tr := tar.NewReader(scan)
	for {
		hdr, err := tr.Next()
		if scan.N <= 0 {
			return nil, errors.New("module tarball exceeds decompressed scan limit before metadata.json")
		}
		if err == io.EOF {
			return nil, errors.New("no metadata.json in module tarball")
		}
		if err != nil {
			return nil, err
		}
		dir, file := path.Split(strings.TrimPrefix(path.Clean(hdr.Name), "./"))
		if hdr.Typeflag != tar.TypeReg || file != "metadata.json" || strings.Count(dir, "/") != 1 {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(tr, maxMetadataJSON))
		if err != nil {
			return nil, err
		}
		var md struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		if err := json.Unmarshal(body, &md); err != nil {
			return nil, fmt.Errorf("parse metadata.json: %w", err)
		}
		if md.Name == "" || md.Version == "" {
			return nil, errors.New("metadata.json missing name or version")
		}
		return &provider.PuppetMetadata{Slug: NormalizeSlug(md.Name), Version: md.Version, MetadataJSON: string(body)}, nil
	}
}

func (p *Provider) ServeLocalIndex(w http.ResponseWriter, r *http.Request, files provider.FileStore, repoName, reqPath string) bool {
	clean := strings.TrimPrefix(path.Clean("/"+reqPath), "/")
	var list func(slug string) ([]provider.PuppetMetadata, error)
	if reader, ok := files.(provider.PuppetMetadataReader); ok {
		list = func(slug string) ([]provider.PuppetMetadata, error) {
			metas, err := reader.ListPuppetMetadataEntries(r.Context(), repoName, slug)
			sortReleases(metas)
			return metas, err
		}
	} else {
		list = func(string) ([]provider.PuppetMetadata, error) {
			return nil, errors.New("puppet metadata not available")
		}
	}

	switch {
	case clean == "v3/releases":
		serveReleaseList(w, r, list)
	case strings.HasPrefix(clean, "v3/releases/"):
		slug, version, ok := ParseRelease(strings.TrimPrefix(clean, "v3/releases/"))
		if !ok {
			NotFound(w)
			return true
		}
		metas, err := list(slug)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return true
		}
		for _, m := range metas {
			if m.Version == version {
				WriteJSON(w, releaseObject(m))
				return true
			}
		}
		NotFound(w)
	case strings.HasPrefix(clean, "v3/modules/"):
		slug := NormalizeSlug(strings.TrimPrefix(clean, "v3/modules/"))
		metas, err := list(slug)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return true
		}
		if len(metas) == 0 {
			NotFound(w)
			return true
		}
		WriteJSON(w, moduleObject(metas))
	default:
		return false
	}
	return true
}

func (p *Provider) GenerateLocalIndex(ctx context.Context, files provider.FileStore, repoName, path string) ([]byte, error) {
	return nil, fmt.Errorf("puppet local index generation for virtual repos not supported")
}

func serveReleaseList(w http.ResponseWriter, r *http.Request, list func(string) ([]provider.PuppetMetadata, error)) {
	q := r.URL.Query()
	slug := NormalizeSlug(q.Get("module"))
	metas, err := list(slug)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	releases := make([]any, len(metas))
	for i, m := range metas {
		releases[i] = releaseObject(m)
	}
	WriteJSON(w, ReleasesPage(slug, releases, q))
}

// ReleasesPage is one limit/offset page of a /v3/releases listing, with
// pagination links relative to the Forge root.
func ReleasesPage(slug string, releases []any, q url.Values) map[string]any {
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit <= 0 {
		limit = 20
	}
	limit = min(limit, 100)
	offset, err := strconv.Atoi(q.Get("offset"))
	if err != nil || offset < 0 {
		offset = 0
	}
	offset = min(offset, len(releases))

	page := func(off int) any {
		if off < 0 || off >= len(releases) && off != 0 {
			return nil
		}
		v := url.Values{"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(off)}}
		if slug != "" {
			v.Set("module", slug)
		}
		return "/v3/releases?" + v.Encode()
	}
	var previous any
	if offset > 0 {
		previous = page(max(offset-limit, 0))
	}
	return map[string]any{
		"pagination": map[string]any{
			"limit":    limit,
			"offset":   offset,
			"first":    page(0),
			"previous": previous,
			"current":  page(offset),
			"next":     page(offset + limit),
			"total":    len(releases),
		},
		"results": releases[offset : offset+min(limit, len(releases)-offset)],
	}
}

func owner(slug string) map[string]any {
	name, _, _ := strings.Cut(slug, "-")
	return map[string]any{"uri": "/v3/users/" + name, "slug": name, "username": name}
}

func moduleRef(slug string) map[string]any {
	_, name, _ := strings.Cut(slug, "-")
	return map[string]any{"uri": "/v3/modules/" + slug, "slug": slug, "name": name, "deprecated_at": nil, "owner": owner(slug)}
}

const forgeTime = "2006-01-02 15:04:05 -0700"

func releaseObject(m provider.PuppetMetadata) map[string]any {
	slug := m.Slug + "-" + m.Version
	return map[string]any{
		"uri":         "/v3/releases/" + slug,
		"slug":        slug,
		"module":      moduleRef(m.Slug),
		"version":     m.Version,
		"metadata":    json.RawMessage(m.MetadataJSON),
		"tags":        []string{},
		"supported":   false,
		"pdk":         false,
		"file_uri":    "/" + m.FilePath,
		"file_size":   m.Size,
		"file_md5":    m.MD5,
		"file_sha256": strings.TrimPrefix(m.ContentHash, "sha256:"),
		"downloads":   0,
		"readme":      nil,
		"changelog":   nil,
		"license":     nil,
		"created_at":  m.CreatedAt.Format(forgeTime),
		"updated_at":  m.CreatedAt.Format(forgeTime),
		"deleted_at":  nil,
	}
}

// moduleObject expects metas sorted newest version first.
func moduleObject(metas []provider.PuppetMetadata) map[string]any {
	obj := moduleRef(metas[0].Slug)
	releases := make([]map[string]any, 0, len(metas))
	for _, m := range metas {
		slug := m.Slug + "-" + m.Version
		releases = append(releases, map[string]any{
			"uri": "/v3/releases/" + slug, "slug": slug, "version": m.Version,
			"file_uri": "/" + m.FilePath, "file_size": m.Size, "supported": false,
			"created_at": m.CreatedAt.Format(forgeTime), "deleted_at": nil,
		})
	}
	obj["current_release"] = releaseObject(metas[0])
	obj["releases"] = releases
	obj["downloads"] = 0
	obj["created_at"] = metas[len(metas)-1].CreatedAt.Format(forgeTime)
	obj["updated_at"] = metas[0].CreatedAt.Format(forgeTime)
	return obj
}

func WriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func NotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `{"message":"404 Not Found","errors":["The requested resource could not be found"]}`)
}

// sortReleases orders by module slug, then newest semver first.
func sortReleases(metas []provider.PuppetMetadata) {
	slices.SortStableFunc(metas, func(a, b provider.PuppetMetadata) int {
		if c := strings.Compare(a.Slug, b.Slug); c != 0 {
			return c
		}
		return CompareSemver(b.Version, a.Version)
	})
}

func CompareSemver(a, b string) int {
	a, _, _ = strings.Cut(a, "+")
	b, _, _ = strings.Cut(b, "+")
	aCore, aPre, aHasPre := strings.Cut(a, "-")
	bCore, bPre, bHasPre := strings.Cut(b, "-")
	if c := compareIdents(strings.Split(aCore, "."), strings.Split(bCore, ".")); c != 0 {
		return c
	}
	switch {
	case aHasPre && !bHasPre:
		return -1
	case !aHasPre && bHasPre:
		return 1
	}
	return compareIdents(strings.Split(aPre, "."), strings.Split(bPre, "."))
}

func compareIdents(a, b []string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		an, aErr := strconv.Atoi(a[i])
		bn, bErr := strconv.Atoi(b[i])
		var c int
		switch {
		case aErr == nil && bErr == nil:
			c = an - bn
		case aErr == nil:
			c = -1
		case bErr == nil:
			c = 1
		default:
			c = strings.Compare(a[i], b[i])
		}
		if c != 0 {
			return c
		}
	}
	return len(a) - len(b)
}
