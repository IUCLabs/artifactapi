package helm

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/IUCLabs/artifactapi/internal/auth"
	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/storage"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func init() {
	provider.Register(&Provider{})
}

type Provider struct{}

func (p *Provider) Type() models.PackageType { return models.PackageHelm }

func (p *Provider) Classify(path string) provider.Mutability {
	if strings.HasSuffix(path, "index.yaml") || strings.HasSuffix(path, "index.yml") {
		return provider.Mutable
	}
	return provider.Immutable
}

func (p *Provider) ContentType(path string) string {
	if strings.HasSuffix(path, ".tgz") || strings.HasSuffix(path, ".tar.gz") {
		return "application/gzip"
	}
	if strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml") {
		return "text/yaml"
	}
	return "application/octet-stream"
}

func (p *Provider) UpstreamURL(remote models.Remote, path string) string {
	return strings.TrimRight(remote.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

func (p *Provider) RewritesResponse() {}

func (p *Provider) RewriteResponse(body []byte, remote models.Remote, proxyBaseURL string) ([]byte, error) {
	if proxyBaseURL == "" {
		return nil, nil
	}
	content := string(body)
	baseURL := strings.TrimRight(remote.BaseURL, "/")
	proxyURL := strings.TrimRight(proxyBaseURL, "/") + "/api/v1/remote/" + remote.Name
	rewritten := strings.ReplaceAll(content, baseURL, proxyURL)
	if rewritten == content {
		return nil, nil
	}
	return []byte(rewritten), nil
}

func (p *Provider) AuthHeaders(_ context.Context, remote models.Remote) (http.Header, error) {
	return auth.BasicHeaders(remote), nil
}

// ValidateUpload stores charts flat by filename so index.yaml can reference
// them with relative urls, as `helm repo index` does.
func (p *Provider) ValidateUpload(filePath string) (storagePath, contentType string, err error) {
	filename := path.Base(path.Clean("/" + filePath))
	if !strings.HasSuffix(filename, ".tgz") || filename == ".tgz" {
		return "", "", fmt.Errorf("file must be a .tgz chart package")
	}
	return filename, "application/gzip", nil
}

func (p *Provider) UploadResponse(storagePath, contentHash string, sizeBytes int64) map[string]any {
	return map[string]any{
		"filename":     storagePath,
		"content_hash": contentHash,
		"size_bytes":   sizeBytes,
	}
}

func (p *Provider) AfterUpload(ctx context.Context, repoName, storagePath, contentHash string, blobs provider.BlobReader, db provider.MetadataStore) {
	reader, _, err := blobs.Download(ctx, storage.BlobKey(strings.TrimPrefix(contentHash, "sha256:")))
	if err != nil {
		slog.Error("helm metadata: download failed", "repo", repoName, "path", storagePath, "error", err)
		return
	}
	defer func() { _ = reader.Close() }()

	meta, err := parseChart(reader)
	if err != nil {
		slog.Error("helm metadata: parse failed", "repo", repoName, "path", storagePath, "error", err)
		return
	}
	if want := meta.Name + "-" + meta.Version + ".tgz"; storagePath != want {
		slog.Error("helm metadata: filename does not match chart", "repo", repoName, "path", storagePath, "expected", want)
		return
	}
	meta.RepoName = repoName
	meta.FilePath = storagePath
	meta.ContentHash = contentHash

	store, ok := db.(provider.HelmMetadataStore)
	if !ok {
		slog.Error("helm metadata: store does not support helm metadata", "repo", repoName)
		return
	}
	if err := store.InsertHelmMetadata(ctx, meta); err != nil {
		slog.Error("helm metadata: insert failed", "repo", repoName, "path", storagePath, "error", err)
		return
	}
	slog.Info("helm metadata: parsed", "repo", repoName, "name", meta.Name, "version", meta.Version)
}

func (p *Provider) AfterDelete(ctx context.Context, repoName, storagePath string, db provider.MetadataDeleter) error {
	deleter, ok := db.(provider.HelmMetadataDeleter)
	if !ok {
		return nil
	}
	return deleter.DeleteHelmMetadata(ctx, repoName, storagePath)
}

func isIndexPath(p string) bool {
	return strings.TrimPrefix(path.Clean("/"+p), "/") == "index.yaml"
}

func (p *Provider) ServeLocalIndex(w http.ResponseWriter, r *http.Request, files provider.FileStore, repoName, reqPath string) bool {
	if !isIndexPath(reqPath) {
		return false
	}
	body, err := p.GenerateLocalIndex(r.Context(), files, repoName, reqPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return true
	}
	w.Header().Set("Content-Type", "text/yaml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	return true
}

func (p *Provider) GenerateLocalIndex(ctx context.Context, files provider.FileStore, repoName, reqPath string) ([]byte, error) {
	if !isIndexPath(reqPath) {
		return nil, fmt.Errorf("unsupported index path: %q", reqPath)
	}
	reader, ok := files.(provider.HelmMetadataReader)
	if !ok {
		return nil, errors.New("helm metadata not available")
	}
	metas, err := reader.ListHelmMetadataEntries(ctx, repoName)
	if err != nil {
		return nil, err
	}
	return generateIndex(metas)
}

// generateIndex builds index.yaml from stored Chart.yaml documents; it is
// stamped from the newest upload so every replica serves identical bytes.
func generateIndex(metas []provider.HelmMetadata) ([]byte, error) {
	entries := map[string][]map[string]yaml.Node{}
	var newest time.Time
	budget := provider.MaxAliasExpandedNodes
	for _, m := range metas {
		var raw map[string]yaml.Node
		if err := yaml.Unmarshal([]byte(m.ChartYAML), &raw); err != nil {
			return nil, fmt.Errorf("chart %s: %w", m.FilePath, err)
		}
		chart, err := provider.ExpandYAMLAliases(raw, &budget)
		if err != nil {
			slog.Warn("helm index: dropping chart", "chart", m.Name, "path", m.FilePath, "error", err)
			continue
		}
		chart["name"] = node(m.Name)
		chart["version"] = node(m.Version)
		chart["urls"] = node([]string{url.PathEscape(m.FilePath)})
		chart["digest"] = node(strings.TrimPrefix(m.ContentHash, "sha256:"))
		chart["created"] = node(m.CreatedAt.UTC().Format(time.RFC3339Nano))
		entries[m.Name] = append(entries[m.Name], chart)
		if m.CreatedAt.After(newest) {
			newest = m.CreatedAt
		}
	}
	return yaml.Marshal(map[string]any{
		"apiVersion": "v1",
		"entries":    entries,
		"generated":  newest.UTC().Format(time.RFC3339Nano),
	})
}

func node(v any) yaml.Node {
	var n yaml.Node
	_ = n.Encode(v)
	return n
}

const (
	maxChartYAML    = 1 << 20
	maxChartBytes   = 50 << 20
	maxChartEntries = 10000
)

var errChartTooLarge = fmt.Errorf("chart archive exceeds %d decompressed bytes", maxChartBytes)

type capReader struct {
	r io.Reader
	n int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.n <= 0 {
		return 0, errChartTooLarge
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	n, err := c.r.Read(p)
	c.n -= int64(n)
	return n, err
}

// parseChart reads the top-level <chart>/Chart.yaml from a packaged chart.
func parseChart(r io.Reader) (*provider.HelmMetadata, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(&capReader{r: zr, n: maxChartBytes})
	for entries := 0; ; entries++ {
		if entries >= maxChartEntries {
			return nil, fmt.Errorf("chart archive exceeds %d entries", maxChartEntries)
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("no Chart.yaml in chart archive")
		}
		if err != nil {
			return nil, err
		}
		dir, file := path.Split(path.Clean(hdr.Name))
		if file != "Chart.yaml" || strings.Count(dir, "/") != 1 {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(tr, maxChartYAML+1))
		if err != nil {
			return nil, err
		}
		if len(raw) > maxChartYAML {
			return nil, fmt.Errorf("parse Chart.yaml: exceeds %d bytes", maxChartYAML)
		}
		var chart struct {
			Name    string `yaml:"name"`
			Version string `yaml:"version"`
		}
		if err := yaml.Unmarshal(raw, &chart); err != nil {
			return nil, fmt.Errorf("parse Chart.yaml: %w", err)
		}
		if chart.Name == "" || chart.Version == "" {
			return nil, errors.New("chart metadata missing name or version")
		}
		return &provider.HelmMetadata{Name: chart.Name, Version: chart.Version, ChartYAML: string(raw)}, nil
	}
}
