package v1

import (
	"net/url"
	"strings"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/provider"
)

type plainProvider struct{ provider.Provider }

type forwardingProvider struct{ plainProvider }

func digits(v string) (string, bool) {
	if v == "" || strings.Trim(v, "0123456789") != "" {
		return "", false
	}
	return strings.TrimLeft(v, "0"), true
}

func (forwardingProvider) ForwardedQuery(path string) map[string]provider.QueryValidator {
	if strings.HasSuffix(path, ".tar.gz") {
		return nil
	}
	return map[string]provider.QueryValidator{"limit": digits, "offset": digits}
}

func TestForwardQuery(t *testing.T) {
	tests := []struct {
		name  string
		prov  provider.Provider
		path  string
		query string
		want  string
	}{
		{"non-forwarder keeps path", plainProvider{}, "a/versions/", "limit=10&offset=5", "a/versions/"},
		{"no query", forwardingProvider{}, "a/versions/", "", "a/versions/"},
		{"allowlisted params sorted", forwardingProvider{}, "a/versions/", "offset=5&limit=10", "a/versions/?limit=10&offset=5"},
		{"unknown params dropped", forwardingProvider{}, "a/versions/", "x=1&limit=10&cb=zz", "a/versions/?limit=10"},
		{"only unknown params", forwardingProvider{}, "a/versions/", "x=1", "a/versions/"},
		{"first value wins", forwardingProvider{}, "a/versions/", "limit=10&limit=20", "a/versions/?limit=10"},
		{"invalid value dropped", forwardingProvider{}, "a/versions/", "limit=1%202&offset=5", "a/versions/?offset=5"},
		{"value canonicalised", forwardingProvider{}, "a/versions/", "limit=0010", "a/versions/?limit=10"},
		{"path without allowlist", forwardingProvider{}, "a/b-1.0.0.tar.gz", "limit=10", "a/b-1.0.0.tar.gz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, err := url.ParseQuery(tt.query)
			if err != nil {
				t.Fatal(err)
			}
			if got := forwardQuery(tt.prov, tt.path, q); got != tt.want {
				t.Errorf("forwardQuery = %q, want %q", got, tt.want)
			}
		})
	}
}
