package deb

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"github.com/IUCLabs/artifactapi/internal/auth"
	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/storage"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func init() {
	provider.Register(&Provider{})
}

// mutableRe marks the apt index surface (both the flat local repo and a proxied
// Debian/Ubuntu mirror's dists/ tree) so the caching engine revalidates it
// instead of freezing it like an immutable .deb.
var mutableRe = []*regexp.Regexp{
	regexp.MustCompile(`(^|/)Packages(\.gz|\.xz|\.bz2)?$`),
	regexp.MustCompile(`(^|/)Sources(\.gz|\.xz|\.bz2)?$`),
	regexp.MustCompile(`(^|/)Release$`),
	regexp.MustCompile(`(^|/)InRelease$`),
	regexp.MustCompile(`(^|/)Release\.gpg$`),
	regexp.MustCompile(`(^|/)Contents-`),
	regexp.MustCompile(`^dists/`),
	regexp.MustCompile(`/by-hash/`),
}

type Provider struct{}

func (p *Provider) Type() models.PackageType { return models.PackageDeb }

func (p *Provider) Classify(path string) provider.Mutability {
	for _, re := range mutableRe {
		if re.MatchString(path) {
			return provider.Mutable
		}
	}
	return provider.Immutable
}

func (p *Provider) ContentType(path string) string {
	switch {
	case strings.HasSuffix(path, ".deb"):
		return "application/vnd.debian.binary-package"
	case strings.HasSuffix(path, ".gz"):
		return "application/gzip"
	case strings.HasSuffix(path, ".xz"):
		return "application/x-xz"
	case strings.HasSuffix(path, "Packages"), strings.HasSuffix(path, "Release"),
		strings.HasSuffix(path, "InRelease"), strings.HasSuffix(path, "Sources"):
		return "text/plain"
	}
	return "application/octet-stream"
}

func (p *Provider) UpstreamURL(remote models.Remote, path string) string {
	return strings.TrimRight(remote.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

func (p *Provider) AuthHeaders(_ context.Context, remote models.Remote) (http.Header, error) {
	return auth.BasicHeaders(remote), nil
}

func (p *Provider) ValidateUpload(filePath string) (storagePath, contentType string, err error) {
	filename := filePath
	if idx := strings.LastIndex(filePath, "/"); idx >= 0 {
		filename = filePath[idx+1:]
	}

	if !strings.HasSuffix(strings.ToLower(filename), ".deb") {
		return "", "", fmt.Errorf("file must be a .deb package")
	}

	return "pool/" + filename, "application/vnd.debian.binary-package", nil
}

func (p *Provider) UploadResponse(storagePath, contentHash string, sizeBytes int64) map[string]any {
	filename := strings.TrimPrefix(storagePath, "pool/")
	return map[string]any{
		"filename":     filename,
		"content_hash": contentHash,
		"size_bytes":   sizeBytes,
	}
}

func (p *Provider) AfterUpload(ctx context.Context, repoName, storagePath, contentHash string, blobs provider.BlobReader, db provider.MetadataStore) {
	s3Key := storage.BlobKey(strings.TrimPrefix(contentHash, "sha256:"))

	reader, blobSize, err := blobs.Download(ctx, s3Key)
	if err != nil {
		slog.Error("deb metadata: download failed", "repo", repoName, "path", storagePath, "error", err)
		return
	}
	defer func() { _ = reader.Close() }()

	raw, err := io.ReadAll(reader)
	if err != nil {
		slog.Error("deb metadata: read failed", "repo", repoName, "path", storagePath, "error", err)
		return
	}

	control, err := extractControl(raw)
	if err != nil {
		slog.Error("deb metadata: parse failed", "repo", repoName, "path", storagePath, "error", err)
		return
	}
	fields := parseControlFields(control)

	sum := md5.Sum(raw)
	meta := &provider.DebMetadata{
		RepoName:     repoName,
		FilePath:     storagePath,
		ContentHash:  contentHash,
		Name:         fields["Package"],
		Version:      fields["Version"],
		Architecture: fields["Architecture"],
		Control:      strings.TrimRight(control, "\n"),
		Size:         blobSize,
		MD5:          hex.EncodeToString(sum[:]),
		SHA256:       strings.TrimPrefix(contentHash, "sha256:"),
	}

	if meta.Name == "" {
		slog.Error("deb metadata: control missing Package field", "repo", repoName, "path", storagePath)
		return
	}

	if err := db.InsertDebMetadata(ctx, meta); err != nil {
		slog.Error("deb metadata: insert failed", "repo", repoName, "path", storagePath, "error", err)
		return
	}

	slog.Info("deb metadata: parsed", "repo", repoName, "name", meta.Name, "version", meta.Version, "arch", meta.Architecture)
}

func (p *Provider) AfterDelete(ctx context.Context, repoName, storagePath string, db provider.MetadataDeleter) error {
	if err := db.DeleteDebMetadata(ctx, repoName, storagePath); err != nil {
		slog.Error("deb metadata: delete failed", "repo", repoName, "path", storagePath, "error", err)
		return err
	}
	slog.Info("deb metadata: deleted", "repo", repoName, "path", storagePath)
	return nil
}

// extractControl reads a .deb (an ar archive), locates the control.tar.* member,
// decompresses it, and returns the raw ./control paragraph. Pure Go: no dpkg.
func extractControl(deb []byte) (string, error) {
	members, err := readAr(deb)
	if err != nil {
		return "", err
	}

	var name string
	var data []byte
	for _, m := range members {
		if strings.HasPrefix(m.name, "control.tar") {
			name = m.name
			data = m.data
			break
		}
	}
	if data == nil {
		return "", errors.New("no control.tar member in .deb")
	}

	tarBytes, err := decompress(name, data)
	if err != nil {
		return "", err
	}

	return readControlParagraph(tarBytes)
}

// readControlParagraph scans a decompressed control.tar and returns the raw
// ./control paragraph. Shared by the local upload path (extractControl) and the
// github_deb ranged-prefix parser.
func readControlParagraph(controlTar []byte) (string, error) {
	tr := tar.NewReader(bytes.NewReader(controlTar))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read control.tar: %w", err)
		}
		clean := strings.TrimPrefix(hdr.Name, "./")
		if clean == "control" {
			b, err := io.ReadAll(tr)
			if err != nil {
				return "", fmt.Errorf("read control file: %w", err)
			}
			return string(b), nil
		}
	}
	return "", errors.New("no ./control in control.tar")
}

func decompress(name string, data []byte) ([]byte, error) {
	switch {
	case strings.HasSuffix(name, ".gz"):
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer func() { _ = zr.Close() }()
		return io.ReadAll(zr)
	case strings.HasSuffix(name, ".xz"):
		xr, err := xz.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		return io.ReadAll(xr)
	case strings.HasSuffix(name, ".zst"):
		zr, err := zstd.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		return io.ReadAll(zr)
	case strings.HasSuffix(name, ".tar"):
		return data, nil
	}
	return nil, fmt.Errorf("unsupported control.tar compression: %s", name)
}

type arMember struct {
	name string
	data []byte
}

// readAr parses the (trivial) Unix ar archive that wraps a .deb. Each member has
// a 60-byte header; the size field is decimal ASCII and data is padded to an
// even offset.
func readAr(data []byte) ([]arMember, error) {
	const magic = "!<arch>\n"
	if len(data) < len(magic) || string(data[:len(magic)]) != magic {
		return nil, errors.New("not an ar archive")
	}
	off := len(magic)

	var members []arMember
	for off+60 <= len(data) {
		hdr := data[off : off+60]
		off += 60

		name := strings.TrimRight(string(hdr[0:16]), " ")
		name = strings.TrimSuffix(name, "/")
		size, err := strconv.ParseInt(strings.TrimSpace(string(hdr[48:58])), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bad ar size for %q: %w", name, err)
		}
		if off+int(size) > len(data) {
			return nil, fmt.Errorf("truncated ar member %q", name)
		}
		members = append(members, arMember{name: name, data: data[off : off+int(size)]})
		off += int(size)
		if size%2 == 1 {
			off++
		}
	}
	return members, nil
}

// parseControlFields reads the single-line fields of an RFC822-style control
// paragraph. Continuation lines (leading whitespace) belong to the previous
// field and are ignored here since only Package/Version/Architecture are read.
func parseControlFields(control string) map[string]string {
	fields := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(control))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		if _, seen := fields[key]; seen {
			continue
		}
		fields[key] = strings.TrimSpace(line[idx+1:])
	}
	return fields
}

// normalizeIndexPath collapses apt's verbatim dist prefix from a flat-repo
// request. For `deb ... <repo>/ ./`, apt appends the "./" dist literally and asks
// for "./Packages" (and "./Release", "./InRelease"); dot-segments must be
// collapsed so the index matcher sees "Packages". A no-op for pool/*.deb paths.
func normalizeIndexPath(p string) string {
	return strings.TrimPrefix(path.Clean("/"+p), "/")
}

func (p *Provider) ServeLocalIndex(w http.ResponseWriter, r *http.Request, files provider.FileStore, repoName, reqPath string) bool {
	path := normalizeIndexPath(reqPath)
	switch path {
	case "Packages", "Packages.gz", "Release":
	default:
		return false
	}

	reader, ok := files.(provider.DebMetadataReader)
	if !ok {
		http.Error(w, "deb metadata not available", http.StatusInternalServerError)
		return true
	}

	metas, err := reader.ListDebMetadataEntries(r.Context(), repoName)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			slog.Warn("deb: metadata read canceled", "repo", repoName, "error", err)
			http.Error(w, "metadata read canceled", http.StatusServiceUnavailable)
			return true
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return true
	}

	switch path {
	case "Packages":
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(generatePackages(metas))
	case "Packages.gz":
		w.Header().Set("Content-Type", "application/gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(gzipBytes(generatePackages(metas)))
	case "Release":
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(generateRelease(metas))
	}
	return true
}

func (p *Provider) GenerateLocalIndex(ctx context.Context, files provider.FileStore, repoName, path string) ([]byte, error) {
	return nil, fmt.Errorf("deb local index generation for virtual repos not supported")
}

// generatePackages emits the flat-repo Packages file: each package's raw control
// stanza followed by the apt-required Filename/Size/MD5sum/SHA256 fields, blank
// line separated.
func generatePackages(metas []provider.DebMetadata) []byte {
	var b bytes.Buffer
	for _, m := range metas {
		b.WriteString(strings.TrimRight(m.Control, "\n"))
		b.WriteString("\n")
		fmt.Fprintf(&b, "Filename: %s\n", m.FilePath)
		fmt.Fprintf(&b, "Size: %d\n", m.Size)
		if m.MD5 != "" {
			fmt.Fprintf(&b, "MD5sum: %s\n", m.MD5)
		}
		if m.SHA256 != "" {
			fmt.Fprintf(&b, "SHA256: %s\n", m.SHA256)
		}
		b.WriteString("\n")
	}
	return b.Bytes()
}

func generateRelease(metas []provider.DebMetadata) []byte {
	packages := generatePackages(metas)
	packagesGz := gzipBytes(packages)

	arches := uniqueArches(metas)

	var b bytes.Buffer
	fmt.Fprintf(&b, "Date: %s\n", releaseDate(metas).Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Architectures: %s\n", strings.Join(arches, " "))
	b.WriteString("Acquire-By-Hash: no\n")

	b.WriteString("MD5Sum:\n")
	writeReleaseEntry(&b, md5Hex(packages), len(packages), "Packages")
	writeReleaseEntry(&b, md5Hex(packagesGz), len(packagesGz), "Packages.gz")

	b.WriteString("SHA256:\n")
	writeReleaseEntry(&b, sha256Hex(packages), len(packages), "Packages")
	writeReleaseEntry(&b, sha256Hex(packagesGz), len(packagesGz), "Packages.gz")

	return b.Bytes()
}

// releaseDate derives the Release Date: from the newest package's persisted
// created_at (in UTC) so the file is byte-identical across the no-affinity
// replicas and across regenerations (issue #117); an empty repo falls back to
// the Unix epoch. This never uses wall clock, which also keeps Date: from
// running ahead of any Valid-Until logic.
func releaseDate(metas []provider.DebMetadata) time.Time {
	newest := time.Unix(0, 0)
	for _, m := range metas {
		if m.CreatedAt.After(newest) {
			newest = m.CreatedAt
		}
	}
	return newest.UTC()
}

func writeReleaseEntry(b *bytes.Buffer, hash string, size int, name string) {
	fmt.Fprintf(b, " %s %d %s\n", hash, size, name)
}

func uniqueArches(metas []provider.DebMetadata) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range metas {
		a := m.Architecture
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}

func gzipBytes(data []byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(data)
	_ = gz.Close()
	return buf.Bytes()
}

func md5Hex(data []byte) string {
	h := md5.Sum(data)
	return hex.EncodeToString(h[:])
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
