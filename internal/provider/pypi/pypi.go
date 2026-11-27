package pypi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/IUCLabs/artifactapi/internal/auth"
	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func init() {
	provider.Register(&Provider{})
}

var fileRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*\.(whl|tar\.gz|zip)$`)
var normalizeRe = regexp.MustCompile(`[-_.]+`)

const filesHost = "https://files.pythonhosted.org/"

var jsonAPIRe = regexp.MustCompile(`^pypi/[^/]+/([^/]+/)?json/?$`)

type Provider struct{}

func (p *Provider) Type() models.PackageType { return models.PackagePyPI }

func (p *Provider) Classify(path string) provider.Mutability {
	if strings.Contains(path, "simple/") || jsonAPIRe.MatchString(path) {
		return provider.Mutable
	}
	return provider.Immutable
}

func (p *Provider) ContentType(path string) string {
	lower := strings.ToLower(path)
	if strings.HasSuffix(lower, ".whl") || strings.HasSuffix(lower, ".zip") {
		return "application/zip"
	}
	if strings.HasSuffix(lower, ".tar.gz") {
		return "application/gzip"
	}
	if strings.Contains(path, "simple/") {
		return "text/html"
	}
	if jsonAPIRe.MatchString(path) {
		return "application/json"
	}
	return "application/octet-stream"
}

func (p *Provider) UpstreamURL(remote models.Remote, path string) string {
	if strings.HasPrefix(path, "simple/") || jsonAPIRe.MatchString(path) {
		return "https://pypi.org/" + path
	}
	return strings.TrimRight(remote.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

// RewriteServed points file links in cached simple (HTML and PEP 691 JSON) and
// JSON API indexes at this remote, so downloads are fetched and cached here.
func (p *Provider) RewriteServed(body []byte, remote models.Remote, proxyBaseURL string) []byte {
	proxyURL := strings.TrimRight(proxyBaseURL, "/") + "/api/v1/remote/" + remote.Name + "/"
	return []byte(strings.NewReplacer(
		filesHost, proxyURL,
		`href="/simple/`, `href="`+proxyURL+`simple/`,
	).Replace(string(body)))
}

func (p *Provider) AuthHeaders(_ context.Context, remote models.Remote) (http.Header, error) {
	return auth.BasicHeaders(remote), nil
}

func normalize(name string) string {
	return strings.ToLower(normalizeRe.ReplaceAllString(name, "-"))
}

func packageFromWheel(filename string) string {
	parts := strings.SplitN(filename, "-", 3)
	if len(parts) < 2 {
		return ""
	}
	return normalize(parts[0])
}

func packageFromSdist(filename string) string {
	name := filename
	for _, suffix := range []string{".tar.gz", ".zip"} {
		if strings.HasSuffix(name, suffix) {
			name = strings.TrimSuffix(name, suffix)
			break
		}
	}
	idx := strings.LastIndex(name, "-")
	if idx <= 0 {
		return ""
	}
	return normalize(name[:idx])
}

func (p *Provider) ValidateUpload(filePath string) (storagePath, contentType string, err error) {
	filename := filePath
	if idx := strings.LastIndex(filePath, "/"); idx >= 0 {
		filename = filePath[idx+1:]
	}

	if !fileRe.MatchString(filename) {
		return "", "", fmt.Errorf("filename %q must be a .whl, .tar.gz, or .zip file", filename)
	}

	var pkgName string
	if strings.HasSuffix(filename, ".whl") {
		pkgName = packageFromWheel(filename)
	} else {
		pkgName = packageFromSdist(filename)
	}
	if pkgName == "" {
		return "", "", fmt.Errorf("cannot parse package name from %q", filename)
	}

	ct := "application/zip"
	if strings.HasSuffix(filename, ".tar.gz") {
		ct = "application/gzip"
	}

	return pkgName + "/" + filename, ct, nil
}

func (p *Provider) UploadResponse(storagePath, contentHash string, sizeBytes int64) map[string]any {
	parts := strings.SplitN(storagePath, "/", 2)
	filename := storagePath
	if len(parts) == 2 {
		filename = parts[1]
	}
	return map[string]any{
		"package":      parts[0],
		"filename":     filename,
		"content_hash": contentHash,
		"size_bytes":   sizeBytes,
	}
}

func (p *Provider) ServeLocalIndex(w http.ResponseWriter, r *http.Request, files provider.FileStore, repoName, path string) bool {
	if path == "simple" || path == "simple/" {
		p.servePackageList(w, r, files, repoName)
		return true
	}

	if strings.HasPrefix(path, "simple/") {
		pkg := strings.TrimPrefix(path, "simple/")
		pkg = strings.TrimSuffix(pkg, "/")
		if pkg != "" && !strings.Contains(pkg, "/") {
			p.servePackageFiles(w, r, files, repoName, pkg)
			return true
		}
	}

	return false
}

func (p *Provider) GenerateLocalIndex(ctx context.Context, files provider.FileStore, repoName, path string) ([]byte, error) {
	if !strings.HasPrefix(path, "simple/") {
		return nil, fmt.Errorf("unsupported index path: %q", path)
	}

	pkg := strings.TrimPrefix(path, "simple/")
	pkg = strings.TrimSuffix(pkg, "/")
	if pkg == "" {
		return p.generatePackageListHTML(ctx, files, repoName)
	}
	return p.generatePackageFilesHTML(ctx, files, repoName, pkg)
}

func (p *Provider) servePackageList(w http.ResponseWriter, r *http.Request, files provider.FileStore, repoName string) {
	body, err := p.generatePackageListHTML(r.Context(), files, repoName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (p *Provider) servePackageFiles(w http.ResponseWriter, r *http.Request, files provider.FileStore, repoName, packageName string) {
	normalized := normalize(packageName)
	prefix := normalized + "/"
	entries, err := files.ListFilesByPrefix(r.Context(), repoName, prefix)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(entries) == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html><body>\n")
	for _, f := range entries {
		filename := strings.TrimPrefix(f.FilePath, normalized+"/")
		hash := strings.TrimPrefix(f.ContentHash, "sha256:")
		fmt.Fprintf(&b, "<a href=\"../../%s/%s#sha256=%s\">%s</a>\n",
			normalized, filename, hash, filename)
	}
	b.WriteString("</body></html>\n")

	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, b.String())
}

func (p *Provider) generatePackageListHTML(ctx context.Context, files provider.FileStore, repoName string) ([]byte, error) {
	packages, err := files.ListPackages(ctx, repoName)
	if err != nil {
		return nil, err
	}

	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html><body>\n")
	for _, pkg := range packages {
		fmt.Fprintf(&b, "<a href=\"%s/\">%s</a>\n", pkg, pkg)
	}
	b.WriteString("</body></html>\n")
	return []byte(b.String()), nil
}

func (p *Provider) generatePackageFilesHTML(ctx context.Context, files provider.FileStore, repoName, packageName string) ([]byte, error) {
	normalized := normalize(packageName)
	prefix := normalized + "/"
	entries, err := files.ListFilesByPrefix(ctx, repoName, prefix)
	if err != nil {
		return nil, err
	}

	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html><body>\n")
	for _, f := range entries {
		filename := strings.TrimPrefix(f.FilePath, normalized+"/")
		hash := strings.TrimPrefix(f.ContentHash, "sha256:")
		fmt.Fprintf(&b, "<a href=\"%s/%s#sha256=%s\">%s</a>\n",
			normalized, filename, hash, filename)
	}
	b.WriteString("</body></html>\n")
	return []byte(b.String()), nil
}
