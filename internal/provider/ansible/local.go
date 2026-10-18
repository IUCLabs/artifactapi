package ansible

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Local ansible repos speak the Galaxy v3 publish and install API. Per version:
//   artifacts/<ns>-<name>-<version>.tar.gz  collection tarball (immutable)
//   _index/<ns>/<name>/<version>            Entry JSON; gates visibility

const (
	IndexPrefix    = "_index/"
	ArtifactPrefix = "artifacts/"
	PublishPath    = "api/v3/artifacts/collections/"
)

var (
	NameRe    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	VersionRe = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)
)

var (
	MaxManifestBytes int64 = 1 << 20
	MaxScanBytes     int64 = 512 << 20
)

// Entry is one published collection version.
type Entry struct {
	Namespace string          `json:"namespace"`
	Name      string          `json:"name"`
	Version   string          `json:"version"`
	SHA256    string          `json:"sha256"`
	Size      int64           `json:"size"`
	Metadata  json.RawMessage `json:"metadata"`
	CreatedAt time.Time       `json:"created_at"`
}

func (e Entry) Filename() string { return e.Namespace + "-" + e.Name + "-" + e.Version + ".tar.gz" }

// IndexPath is the entry's hidden index file.
func (e Entry) IndexPath() string { return IndexPrefix + e.Namespace + "/" + e.Name + "/" + e.Version }

// TaskID names the import task returned on publish.
func (e Entry) TaskID() string { return e.Namespace + "-" + e.Name + "-" + e.Version }

// ParseCollection reads collection_info from a collection tarball's top-level
// MANIFEST.json. Every member is checked, so a tarball whose paths or links
// escape the collection root is rejected even after the manifest.
func ParseCollection(r io.Reader) (*Entry, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("not a gzip tarball: %w", err)
	}
	scan := &io.LimitedReader{R: zr, N: MaxScanBytes}
	tr := tar.NewReader(scan)
	var e *Entry
	for {
		hdr, err := tr.Next()
		if scan.N <= 0 {
			return nil, errors.New("collection exceeds decompressed size limit")
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tarball: %w", err)
		}
		name, err := memberPath(hdr)
		if err != nil {
			return nil, err
		}
		if name != "MANIFEST.json" || hdr.Typeflag != tar.TypeReg {
			continue
		}
		if e != nil {
			return nil, errors.New("duplicate MANIFEST.json")
		}
		if e, err = parseManifest(io.LimitReader(tr, MaxManifestBytes+1)); err != nil {
			return nil, err
		}
	}
	if e == nil {
		return nil, errors.New("no MANIFEST.json in collection tarball")
	}
	return e, nil
}

// memberPath returns hdr's cleaned name, rejecting absolute or escaping
// paths and links, and anything but files, directories and symlinks.
func memberPath(hdr *tar.Header) (string, error) {
	name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
	if escapes(name) {
		return "", fmt.Errorf("tarball member %q escapes the collection root", hdr.Name)
	}
	switch hdr.Typeflag {
	case tar.TypeReg, tar.TypeDir:
	case tar.TypeSymlink:
		if path.IsAbs(hdr.Linkname) || escapes(path.Join(path.Dir(name), hdr.Linkname)) {
			return "", fmt.Errorf("tarball symlink %q escapes the collection root", hdr.Name)
		}
	default:
		return "", fmt.Errorf("tarball member %q has unsupported type %q", hdr.Name, hdr.Typeflag)
	}
	return name, nil
}

func escapes(p string) bool {
	return path.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../") || strings.Contains(p, "\\")
}

func parseManifest(r io.Reader) (*Entry, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read MANIFEST.json: %w", err)
	}
	if int64(len(raw)) > MaxManifestBytes {
		return nil, fmt.Errorf("MANIFEST.json exceeds %d bytes", MaxManifestBytes)
	}
	var doc struct {
		CollectionInfo map[string]json.RawMessage `json:"collection_info"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.CollectionInfo == nil {
		return nil, errors.New("MANIFEST.json has no collection_info")
	}
	info := doc.CollectionInfo
	e := &Entry{}
	for k, dst := range map[string]*string{"namespace": &e.Namespace, "name": &e.Name, "version": &e.Version} {
		if err := json.Unmarshal(info[k], dst); err != nil {
			return nil, fmt.Errorf("collection_info.%s is not a string", k)
		}
	}
	if !NameRe.MatchString(e.Namespace) || !NameRe.MatchString(e.Name) || !VersionRe.MatchString(e.Version) {
		return nil, fmt.Errorf("invalid collection %q.%q version %q", e.Namespace, e.Name, e.Version)
	}
	deps := map[string]string{}
	if d := info["dependencies"]; len(d) > 0 && string(d) != "null" {
		if err := json.Unmarshal(d, &deps); err != nil {
			return nil, errors.New("collection_info.dependencies must map collection names to version ranges")
		}
	}
	for k := range deps {
		ns, n, ok := strings.Cut(k, ".")
		if !ok || !NameRe.MatchString(ns) || !NameRe.MatchString(n) {
			return nil, fmt.Errorf("invalid dependency %q", k)
		}
	}
	info["dependencies"], _ = json.Marshal(deps)
	if e.Metadata, err = json.Marshal(info); err != nil {
		return nil, err
	}
	return e, nil
}

// APIPath is a parsed Galaxy v3 request path below a repo.
type APIPath struct {
	Kind                     APIKind
	Namespace, Name, Version string
	TaskID                   string
}

type APIKind int

const (
	APIUnknown APIKind = iota
	APIRoot
	APICollection
	APIVersions
	APIVersion
	APIImport
)

// ParseAPIPath parses api/, api/v3/collections/<ns>/<name>/[versions/[<v>/]]
// and api/v3/imports/collections/<id>/, with or without trailing slashes.
func ParseAPIPath(p string) APIPath {
	p = strings.TrimSuffix(p, "/")
	if p == "api" {
		return APIPath{Kind: APIRoot}
	}
	if id, ok := strings.CutPrefix(p, "api/v3/imports/collections/"); ok && id != "" && !strings.Contains(id, "/") {
		return APIPath{Kind: APIImport, TaskID: id}
	}
	rest, ok := strings.CutPrefix(p, "api/v3/collections/")
	if !ok {
		return APIPath{}
	}
	segs := strings.Split(rest, "/")
	if len(segs) < 2 || !NameRe.MatchString(segs[0]) || !NameRe.MatchString(segs[1]) {
		return APIPath{}
	}
	ap := APIPath{Namespace: segs[0], Name: segs[1]}
	switch {
	case len(segs) == 2:
		ap.Kind = APICollection
	case len(segs) == 3 && segs[2] == "versions":
		ap.Kind = APIVersions
	case len(segs) == 4 && segs[2] == "versions" && VersionRe.MatchString(segs[3]):
		ap.Kind, ap.Version = APIVersion, segs[3]
	default:
		return APIPath{}
	}
	return ap
}

// Root is the API root ansible-galaxy discovers.
func Root() map[string]any {
	return map[string]any{"available_versions": map[string]string{"v3": "v3/"}}
}

func CollectionPath(ns, name string) string { return "/api/v3/collections/" + ns + "/" + name + "/" }

func VersionPath(ns, name, version string) string {
	return CollectionPath(ns, name) + "versions/" + url.PathEscape(version) + "/"
}

// VersionRef is one entry of a versions list.
type VersionRef struct {
	Version   string `json:"version"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

func (e Entry) Ref() VersionRef {
	t := e.CreatedAt.UTC().Format(time.RFC3339Nano)
	return VersionRef{Version: e.Version, CreatedAt: t, UpdatedAt: t}
}

// CollectionDoc describes a collection from its versions; base is the repo
// URL the hrefs are built on.
func CollectionDoc(base, ns, name string, refs []VersionRef) map[string]any {
	var created, updated time.Time
	for _, r := range refs {
		if t, err := time.Parse(time.RFC3339Nano, r.CreatedAt); err == nil && (created.IsZero() || t.Before(created)) {
			created = t
		}
		if t, err := time.Parse(time.RFC3339Nano, r.UpdatedAt); err == nil && t.After(updated) {
			updated = t
		}
	}
	return map[string]any{
		"href":         base + CollectionPath(ns, name),
		"namespace":    ns,
		"name":         name,
		"deprecated":   false,
		"versions_url": base + CollectionPath(ns, name) + "versions/",
		"created_at":   formatTime(created),
		"updated_at":   formatTime(updated),
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// Page reads limit/offset as the remote forwards them, defaulting to 100/0.
func Page(q url.Values) (limit, offset int) {
	limit, offset = maxPageLimit, 0
	if v, ok := pageLimit(q.Get("limit")); ok {
		limit, _ = strconv.Atoi(v)
	}
	if v, ok := pageOffset(q.Get("offset")); ok {
		offset, _ = strconv.Atoi(v)
	}
	return limit, offset
}

// VersionsPage is one page of refs in Galaxy v3 list form. Links are
// root-relative like Galaxy's, since ansible-galaxy splices them onto the
// server URL.
func VersionsPage(base, ns, name string, refs []VersionRef, limit, offset int) map[string]any {
	list := rootPath(base) + CollectionPath(ns, name) + "versions/"
	link := func(off int) any {
		return list + "?" + url.Values{"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(off)}}.Encode()
	}
	offset = min(offset, len(refs))
	var previous, next any
	if offset > 0 {
		previous = link(max(offset-limit, 0))
	}
	if offset+limit < len(refs) {
		next = link(offset + limit)
	}
	data := []map[string]any{}
	for _, r := range refs[offset:min(offset+limit, len(refs))] {
		data = append(data, map[string]any{
			"version":    r.Version,
			"href":       base + VersionPath(ns, name, r.Version),
			"created_at": r.CreatedAt,
			"updated_at": r.UpdatedAt,
			"marks":      []string{},
		})
	}
	return map[string]any{
		"meta":  map[string]any{"count": len(refs)},
		"links": map[string]any{"first": link(0), "previous": previous, "next": next, "last": link(max(len(refs)-1, 0) / limit * limit)},
		"data":  data,
	}
}

// rootPath drops the scheme and host from base.
func rootPath(base string) string {
	if u, err := url.Parse(base); err == nil {
		return u.EscapedPath()
	}
	return base
}

// VersionDoc is a version's detail; the tarball is served from the repo at
// artifacts/<filename>.
func VersionDoc(base string, e Entry) map[string]any {
	t := e.CreatedAt.UTC().Format(time.RFC3339Nano)
	return map[string]any{
		"href":         base + VersionPath(e.Namespace, e.Name, e.Version),
		"namespace":    map[string]string{"name": e.Namespace},
		"collection":   map[string]string{"name": e.Name},
		"name":         e.Name,
		"version":      e.Version,
		"download_url": base + "/" + ArtifactPrefix + e.Filename(),
		"artifact":     map[string]any{"filename": e.Filename(), "sha256": e.SHA256, "size": e.Size},
		"metadata":     e.Metadata,
		"signatures":   []string{},
		"created_at":   t,
		"updated_at":   t,
	}
}

// ImportTask is a finished import: publishing completes synchronously.
func ImportTask(e Entry) map[string]any {
	t := e.CreatedAt.UTC().Format(time.RFC3339Nano)
	return map[string]any{
		"id": e.TaskID(), "state": "completed", "created_at": t, "started_at": t, "finished_at": t,
		"messages": []any{}, "error": nil,
	}
}
