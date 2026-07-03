package terraform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/IUCLabs/artifactapi/internal/auth"
	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func init() {
	provider.Register(&Provider{})
}

var versionsRe = regexp.MustCompile(`[^/]+/[^/]+/versions$`)

var providerZipRe = regexp.MustCompile(
	`^terraform-provider-([a-zA-Z0-9_-]+)_([0-9]+\.[0-9]+\.[0-9]+(?:-[a-zA-Z0-9.]+)?)_([a-z0-9]+)_([a-z0-9]+)\.zip$`,
)

var semverRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[a-zA-Z0-9.]+)?$`)

// ParsedProviderZip describes a terraform-provider-{type}_{version}_{os}_{arch}.zip
// filename. Ok is false when the name doesn't match that convention.
type ParsedProviderZip struct {
	Type    string
	Version string
	OS      string
	Arch    string
	Ok      bool
}

// ParseProviderZip extracts the type, version and platform from a provider zip
// filename (the base name, not a full path). It's the canonical parser shared by
// the network-mirror index and the provider registry handler.
func ParseProviderZip(filename string) ParsedProviderZip {
	m := providerZipRe.FindStringSubmatch(filename)
	if m == nil {
		return ParsedProviderZip{}
	}
	return ParsedProviderZip{Type: m[1], Version: m[2], OS: m[3], Arch: m[4], Ok: true}
}

type Provider struct{}

func (p *Provider) Type() models.PackageType { return models.PackageTerraform }

func (p *Provider) Classify(path string) provider.Mutability {
	if versionsRe.MatchString(path) {
		return provider.Mutable
	}
	return provider.Immutable
}

func (p *Provider) ContentType(path string) string {
	lower := strings.ToLower(path)
	if strings.HasSuffix(lower, ".zip") {
		return "application/zip"
	}
	if strings.HasSuffix(lower, ".sig") {
		return "application/octet-stream"
	}
	return "application/json"
}

func (p *Provider) UpstreamURL(remote models.Remote, path string) string {
	return strings.TrimRight(remote.BaseURL, "/") + "/v1/providers/" + strings.TrimLeft(path, "/")
}

func (p *Provider) RewriteResponse(body []byte, remote models.Remote, proxyBaseURL string) ([]byte, error) {
	if remote.ReleasesRemote == "" {
		return nil, nil
	}
	if !json.Valid(body) {
		return nil, nil
	}

	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, nil
	}

	changed := false
	for _, field := range []string{"download_url", "shasums_url", "shasums_signature_url"} {
		if val, ok := data[field].(string); ok && val != "" {
			rewritten := rewriteDownloadURL(val, remote.ReleasesRemote, proxyBaseURL)
			if rewritten != val {
				data[field] = rewritten
				changed = true
			}
		}
	}

	if !changed {
		return nil, nil
	}
	return json.Marshal(data)
}

func rewriteDownloadURL(originalURL, releasesRemote, proxyBaseURL string) string {
	parsed, err := url.Parse(originalURL)
	if err != nil || proxyBaseURL == "" {
		return originalURL
	}
	return strings.TrimRight(proxyBaseURL, "/") + "/api/v1/remote/" + releasesRemote + parsed.Path
}

func (p *Provider) AuthHeaders(_ context.Context, remote models.Remote) (http.Header, error) {
	return auth.BasicHeaders(remote), nil
}

func (p *Provider) ValidateUpload(filePath string) (storagePath, contentType string, err error) {
	parts := strings.Split(filePath, "/")
	if len(parts) != 3 {
		return "", "", fmt.Errorf("path must be {namespace}/{type}/{filename}.zip")
	}
	namespace, typeName, filename := parts[0], parts[1], parts[2]

	m := providerZipRe.FindStringSubmatch(filename)
	if m == nil {
		return "", "", fmt.Errorf("filename %q does not match terraform-provider-{type}_{version}_{os}_{arch}.zip", filename)
	}

	if m[1] != typeName {
		return "", "", fmt.Errorf("provider type in filename %q does not match path type %q", m[1], typeName)
	}

	return fmt.Sprintf("%s/%s/%s", namespace, typeName, filename), "application/zip", nil
}

func (p *Provider) UploadResponse(storagePath, contentHash string, sizeBytes int64) map[string]any {
	parts := strings.Split(storagePath, "/")
	if len(parts) != 3 {
		return map[string]any{"path": storagePath, "content_hash": contentHash, "size_bytes": sizeBytes}
	}

	m := providerZipRe.FindStringSubmatch(parts[2])
	if m == nil {
		return map[string]any{"path": storagePath, "content_hash": contentHash, "size_bytes": sizeBytes}
	}

	return map[string]any{
		"namespace":    parts[0],
		"type":         parts[1],
		"version":      m[2],
		"os":           m[3],
		"arch":         m[4],
		"content_hash": contentHash,
		"size_bytes":   sizeBytes,
	}
}

type terraformIndex struct {
	Versions map[string]json.RawMessage `json:"versions"`
}

type terraformVersionDoc struct {
	Archives map[string]terraformArchive `json:"archives"`
}

type terraformArchive struct {
	URL    string   `json:"url"`
	Hashes []string `json:"hashes,omitempty"`
}

func (p *Provider) ServeLocalIndex(w http.ResponseWriter, r *http.Request, files provider.FileStore, repoName, path string) bool {
	parts := strings.Split(path, "/")
	if len(parts) < 3 {
		return false
	}

	namespace, typeName := parts[0], parts[1]
	tail := parts[2]

	if tail == "index.json" {
		p.serveIndex(w, r, files, repoName, namespace, typeName)
		return true
	}

	if strings.HasSuffix(tail, ".json") {
		version := strings.TrimSuffix(tail, ".json")
		if semverRe.MatchString(version) {
			p.serveVersionDoc(w, r, files, repoName, namespace, typeName, version)
			return true
		}
	}

	return false
}

func (p *Provider) GenerateLocalIndex(ctx context.Context, files provider.FileStore, repoName, path string) ([]byte, error) {
	return nil, fmt.Errorf("terraform local index generation for virtual repos not supported")
}

func (p *Provider) serveIndex(w http.ResponseWriter, r *http.Request, files provider.FileStore, repoName, namespace, typeName string) {
	prefix := fmt.Sprintf("%s/%s/", namespace, typeName)
	entries, err := files.ListFilesByPrefix(r.Context(), repoName, prefix)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	versions := map[string]json.RawMessage{}
	for _, f := range entries {
		filename := strings.TrimPrefix(f.FilePath, prefix)
		m := providerZipRe.FindStringSubmatch(filename)
		if m == nil {
			continue
		}
		versions[m[2]] = json.RawMessage(`{}`)
	}

	if len(versions) == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(terraformIndex{Versions: versions})
}

func (p *Provider) serveVersionDoc(w http.ResponseWriter, r *http.Request, files provider.FileStore, repoName, namespace, typeName, version string) {
	prefix := fmt.Sprintf("%s/%s/terraform-provider-%s_%s_", namespace, typeName, typeName, version)
	entries, err := files.ListFilesByPrefix(r.Context(), repoName, prefix)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	archives := map[string]terraformArchive{}
	for _, f := range entries {
		filename := strings.TrimPrefix(f.FilePath, fmt.Sprintf("%s/%s/", namespace, typeName))
		m := providerZipRe.FindStringSubmatch(filename)
		if m == nil || m[2] != version {
			continue
		}
		platform := m[3] + "_" + m[4]
		archive := terraformArchive{URL: filename}
		if f.ContentHash != "" {
			archive.Hashes = []string{"zh:" + strings.TrimPrefix(f.ContentHash, "sha256:")}
		}
		archives[platform] = archive
	}

	if len(archives) == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(terraformVersionDoc{Archives: archives})
}
