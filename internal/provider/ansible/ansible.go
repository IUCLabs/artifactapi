// Package ansible proxies an Ansible Galaxy server (v3 collections API):
// mutable JSON metadata whose links are rewritten back to the remote, plus
// immutable collection tarballs.
package ansible

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/IUCLabs/artifactapi/internal/auth"
	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func init() {
	provider.Register(&Provider{})
}

type Provider struct{}

func (p *Provider) Type() models.PackageType { return models.PackageAnsible }

func (p *Provider) Classify(path string) provider.Mutability {
	if isArtifact(path) {
		return provider.Immutable
	}
	return provider.Mutable
}

func (p *Provider) ContentType(path string) string {
	if isArtifact(path) {
		return "application/gzip"
	}
	return "application/json"
}

func (p *Provider) UpstreamURL(remote models.Remote, path string) string {
	return strings.TrimRight(remote.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

const maxPageLimit = 100

// ForwardedQuery keeps limit/offset pagination on metadata paths only.
func (p *Provider) ForwardedQuery(path string) map[string]provider.QueryValidator {
	if isArtifact(path) {
		return nil
	}
	return map[string]provider.QueryValidator{"limit": pageLimit, "offset": pageOffset}
}

func pageOffset(v string) (string, bool) {
	n, err := strconv.ParseUint(v, 10, 32)
	return strconv.FormatUint(n, 10), err == nil
}

func pageLimit(v string) (string, bool) {
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil || n == 0 {
		return "", false
	}
	return strconv.FormatUint(min(n, maxPageLimit), 10), true
}

// RewriteResponse points href, download_url and links.* at the remote. Links
// stay root-relative, which ansible-galaxy joins onto the server URL, so the
// cached body is independent of the request host.
func (p *Provider) RewriteResponse(body []byte, remote models.Remote, _ string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, nil
	}
	r := rewriter{
		prefix: provider.RemoteMountPath + url.PathEscape(remote.Name) + "/",
		base:   strings.TrimRight(remote.BaseURL, "/") + "/",
	}
	r.walk(doc)
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (p *Provider) AuthHeaders(_ context.Context, remote models.Remote) (http.Header, error) {
	return auth.BasicHeaders(remote), nil
}

type rewriter struct{ prefix, base string }

func (r rewriter) walk(v any) {
	switch v := v.(type) {
	case []any:
		for _, e := range v {
			r.walk(e)
		}
	case map[string]any:
		for k, e := range v {
			switch k {
			case "href", "download_url":
				v[k] = r.link(e)
			case "links":
				if links, ok := e.(map[string]any); ok {
					for lk, l := range links {
						links[lk] = r.link(l)
					}
					continue
				}
			}
			r.walk(v[k])
		}
	}
}

func (r rewriter) link(v any) any {
	s, ok := v.(string)
	switch {
	case !ok:
		return v
	case strings.HasPrefix(s, "/api/"):
		return r.prefix + s[1:]
	case strings.HasPrefix(s, r.base):
		return r.prefix + strings.TrimPrefix(s, r.base)
	}
	return s
}

func isArtifact(path string) bool {
	path, _, _ = strings.Cut(path, "?")
	return strings.Contains(path, "/artifacts/") && strings.HasSuffix(path, ".tar.gz")
}
