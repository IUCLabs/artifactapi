package alpine

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"

	"archive/tar"

	"github.com/IUCLabs/artifactapi/internal/auth"
	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/storage"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func init() {
	provider.Register(&Provider{})
}

type Provider struct{}

func (p *Provider) Type() models.PackageType { return models.PackageAlpine }

func (p *Provider) Classify(path string) provider.Mutability {
	if strings.HasSuffix(path, "APKINDEX.tar.gz") {
		return provider.Mutable
	}
	return provider.Immutable
}

func (p *Provider) ContentType(path string) string {
	if strings.HasSuffix(path, ".apk") {
		return "application/vnd.android.package-archive"
	}
	if strings.HasSuffix(path, ".tar.gz") {
		return "application/gzip"
	}
	return "application/octet-stream"
}

func (p *Provider) UpstreamURL(remote models.Remote, path string) string {
	return strings.TrimRight(remote.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

func (p *Provider) RewriteResponse(_ []byte, _ models.Remote, _ string) ([]byte, error) {
	return nil, nil
}

func (p *Provider) AuthHeaders(_ context.Context, remote models.Remote) (http.Header, error) {
	return auth.BasicHeaders(remote), nil
}

// --- LocalUploader: hosting real .apk packages -----------------------------

// ValidateUpload accepts any *.apk and preserves the client-supplied directory
// (the arch prefix) as the storage path, since arch cannot be parsed from the
// filename alone and the generic uploader hands us only the path. apk clients
// fetch packages at <arch>/<file>.apk, so publishers upload to that same path;
// AfterUpload records the true arch (from .PKGINFO) for index filtering.
func (p *Provider) ValidateUpload(filePath string) (storagePath, contentType string, err error) {
	clean := strings.TrimPrefix(path.Clean("/"+filePath), "/")
	filename := clean
	if i := strings.LastIndex(clean, "/"); i >= 0 {
		filename = clean[i+1:]
	}
	if !strings.HasSuffix(strings.ToLower(filename), ".apk") {
		return "", "", fmt.Errorf("file must be a .apk package")
	}
	return clean, "application/vnd.android.package-archive", nil
}

func (p *Provider) UploadResponse(storagePath, contentHash string, sizeBytes int64) map[string]any {
	filename := storagePath
	if i := strings.LastIndex(storagePath, "/"); i >= 0 {
		filename = storagePath[i+1:]
	}
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
		slog.Error("alpine metadata: download failed", "repo", repoName, "path", storagePath, "error", err)
		return
	}
	defer reader.Close()

	raw, err := io.ReadAll(reader)
	if err != nil {
		slog.Error("alpine metadata: read failed", "repo", repoName, "path", storagePath, "error", err)
		return
	}

	meta, err := parseApk(raw)
	if err != nil {
		slog.Error("alpine metadata: parse failed", "repo", repoName, "path", storagePath, "error", err)
		return
	}
	meta.RepoName = repoName
	meta.FilePath = storagePath
	meta.ContentHash = contentHash
	meta.DownloadSize = blobSize

	if meta.Name == "" || meta.Arch == "" {
		slog.Error("alpine metadata: .PKGINFO missing pkgname/arch", "repo", repoName, "path", storagePath)
		return
	}

	store, ok := db.(provider.AlpineMetadataStore)
	if !ok {
		slog.Error("alpine metadata: store does not support alpine metadata", "repo", repoName)
		return
	}
	if err := store.InsertAlpineMetadata(ctx, meta); err != nil {
		slog.Error("alpine metadata: insert failed", "repo", repoName, "path", storagePath, "error", err)
		return
	}
	slog.Info("alpine metadata: parsed", "repo", repoName, "name", meta.Name, "version", meta.Version, "arch", meta.Arch)
}

func (p *Provider) AfterDelete(ctx context.Context, repoName, storagePath string, db provider.MetadataDeleter) error {
	deleter, ok := db.(provider.AlpineMetadataDeleter)
	if !ok {
		return nil
	}
	if err := deleter.DeleteAlpineMetadata(ctx, repoName, storagePath); err != nil {
		slog.Error("alpine metadata: delete failed", "repo", repoName, "path", storagePath, "error", err)
		return err
	}
	slog.Info("alpine metadata: deleted", "repo", repoName, "path", storagePath)
	return nil
}

// --- LocalIndexer: generating a per-arch APKINDEX.tar.gz -------------------

// normalizeIndexPath collapses apk's dot-segment prefix: an /etc/apk/repositories
// line of "<url>/api/v1/local/<name>" makes apk request "./<arch>/APKINDEX.tar.gz".
// Mirrors deb's flat-repo normalization.
func normalizeIndexPath(p string) string {
	return strings.TrimPrefix(path.Clean("/"+p), "/")
}

func (p *Provider) ServeLocalIndex(w http.ResponseWriter, r *http.Request, files provider.FileStore, repoName, reqPath string) bool {
	clean := normalizeIndexPath(reqPath)
	if !strings.HasSuffix(clean, "APKINDEX.tar.gz") {
		return false
	}
	arch := strings.TrimSuffix(clean, "APKINDEX.tar.gz")
	arch = strings.Trim(arch, "/")
	if arch == "" || strings.Contains(arch, "/") {
		http.Error(w, "APKINDEX must be requested per-arch: <arch>/APKINDEX.tar.gz", http.StatusNotFound)
		return true
	}

	reader, ok := files.(provider.AlpineMetadataReader)
	if !ok {
		http.Error(w, "alpine metadata not available", http.StatusInternalServerError)
		return true
	}

	metas, err := reader.ListAlpineMetadataEntries(r.Context(), repoName)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			slog.Warn("alpine: metadata read canceled", "repo", repoName, "error", err)
			http.Error(w, "metadata read canceled", http.StatusServiceUnavailable)
			return true
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return true
	}

	var filtered []provider.AlpineMetadata
	for _, m := range metas {
		if m.Arch == arch {
			filtered = append(filtered, m)
		}
	}

	w.Header().Set("Content-Type", "application/gzip")
	w.WriteHeader(http.StatusOK)
	w.Write(generateAPKIndex(filtered))
	return true
}

func (p *Provider) GenerateLocalIndex(ctx context.Context, files provider.FileStore, repoName, path string) ([]byte, error) {
	return nil, fmt.Errorf("alpine local index generation for virtual repos not supported")
}

// --- pure-Go .apk parsing --------------------------------------------------

// parseApk reads an .apk (up to three concatenated, independently gzipped tar
// streams: optional signature, control, data). It locates the control stream by
// its .PKGINFO member, computes the apk pull checksum C: = "Q1" +
// base64(sha1(<control gzip stream bytes>)), and reads the .PKGINFO fields.
func parseApk(raw []byte) (*provider.AlpineMetadata, error) {
	members, err := gzipMembers(raw)
	if err != nil {
		return nil, err
	}

	for _, m := range members {
		pkginfo, ok := pkginfoFromTar(m.tar)
		if !ok {
			continue
		}
		meta := parsePkginfo(pkginfo)
		sum := sha1.Sum(m.raw)
		meta.Checksum = "Q1" + base64.StdEncoding.EncodeToString(sum[:])
		return meta, nil
	}
	return nil, errors.New("no .PKGINFO found in any .apk gzip stream")
}

type gzMember struct {
	raw []byte // the raw bytes of this gzip stream (for the Q1 checksum)
	tar []byte // the decompressed tar payload
}

// gzipMembers splits the concatenated gzip streams, returning each stream's raw
// bytes alongside its decompressed tar. It relies on bytes.Reader being an
// io.ByteReader (so compress/gzip does not over-read past a member's trailer)
// to recover exact stream boundaries via Multistream(false)+Reset.
func gzipMembers(data []byte) ([]gzMember, error) {
	br := bytes.NewReader(data)
	zr, err := gzip.NewReader(br)
	if err != nil {
		return nil, err
	}
	var members []gzMember
	prev := 0
	for {
		zr.Multistream(false)
		out, err := io.ReadAll(zr)
		if err != nil {
			return nil, err
		}
		end := len(data) - br.Len()
		members = append(members, gzMember{raw: data[prev:end], tar: out})
		prev = end
		if err := zr.Reset(br); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
	}
	return members, nil
}

func pkginfoFromTar(tarBytes []byte) (string, bool) {
	tr := tar.NewReader(bytes.NewReader(tarBytes))
	for {
		hdr, err := tr.Next()
		if err != nil {
			return "", false
		}
		if strings.TrimPrefix(hdr.Name, "./") == ".PKGINFO" {
			b, err := io.ReadAll(tr)
			if err != nil {
				return "", false
			}
			return string(b), true
		}
	}
}

// parsePkginfo reads the "key = value" .PKGINFO text, collecting the repeated
// depend/provides/install_if keys into slices.
func parsePkginfo(text string) *provider.AlpineMetadata {
	m := &provider.AlpineMetadata{}
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		switch key {
		case "pkgname":
			m.Name = val
		case "pkgver":
			m.Version = val
		case "arch":
			m.Arch = val
		case "pkgdesc":
			m.Description = val
		case "url":
			m.URL = val
		case "license":
			m.License = val
		case "origin":
			m.Origin = val
		case "maintainer":
			m.Maintainer = val
		case "builddate":
			if n, err := strconv.ParseInt(val, 10, 64); err == nil {
				m.BuildTime = n
			}
		case "commit":
			m.Commit = val
		case "size":
			if n, err := strconv.ParseInt(val, 10, 64); err == nil {
				m.InstalledSize = n
			}
		case "provider_priority":
			m.ProviderPriority = val
		case "depend":
			if val != "" {
				m.Depends = append(m.Depends, val)
			}
		case "provides":
			if val != "" {
				m.Provides = append(m.Provides, val)
			}
		case "install_if":
			if val != "" {
				m.InstallIf = append(m.InstallIf, val)
			}
		}
	}
	return m
}

// generateAPKIndex builds the APKINDEX.tar.gz = gzip(tar(APKINDEX)) for the
// given (already arch-filtered) rows. Records are blank-line separated; fields
// follow the canonical C/P/V/A/S/I/T/U/L/o/m/t/c/k/D/p/i order and empties are
// omitted. Unsigned (clients use --allow-untrusted), matching rpm gpgcheck=0.
func generateAPKIndex(metas []provider.AlpineMetadata) []byte {
	var idx bytes.Buffer
	for i, m := range metas {
		if i > 0 {
			idx.WriteString("\n")
		}
		writeField(&idx, "C", m.Checksum)
		writeField(&idx, "P", m.Name)
		writeField(&idx, "V", m.Version)
		writeField(&idx, "A", m.Arch)
		writeField(&idx, "S", intField(m.DownloadSize))
		writeField(&idx, "I", intField(m.InstalledSize))
		writeField(&idx, "T", m.Description)
		writeField(&idx, "U", m.URL)
		writeField(&idx, "L", m.License)
		writeField(&idx, "o", m.Origin)
		writeField(&idx, "m", m.Maintainer)
		writeField(&idx, "t", intField(m.BuildTime))
		writeField(&idx, "c", m.Commit)
		writeField(&idx, "k", m.ProviderPriority)
		writeField(&idx, "D", strings.Join(m.Depends, " "))
		writeField(&idx, "p", strings.Join(m.Provides, " "))
		writeField(&idx, "i", strings.Join(m.InstallIf, " "))
	}

	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	body := idx.Bytes()
	tw.WriteHeader(&tar.Header{Name: "APKINDEX", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write(body)
	tw.Close()

	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	gz.Write(tarBuf.Bytes())
	gz.Close()
	return gzBuf.Bytes()
}

func writeField(b *bytes.Buffer, key, val string) {
	if val == "" {
		return
	}
	b.WriteString(key)
	b.WriteString(":")
	b.WriteString(val)
	b.WriteString("\n")
}

func intField(n int64) string {
	if n == 0 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}
