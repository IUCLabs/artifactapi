package goproxy

import (
	"context"
	"testing"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

func TestGoProxyURLAuth(t *testing.T) {
	p := &Provider{}
	if got := p.UpstreamURL(models.Remote{BaseURL: "https://proxy.golang.org/"}, "/mod/@v/list"); got != "https://proxy.golang.org/mod/@v/list" {
		t.Errorf("upstream url %q", got)
	}
	if h, _ := p.AuthHeaders(context.Background(), models.Remote{Username: "u", Password: "p"}); h.Get("Authorization") == "" {
		t.Error("expected basic auth header")
	}
	if got := p.ContentType("mod/@v/v1.0.0.info"); got != "application/json" {
		t.Errorf("info content type %q", got)
	}
	if got := p.ContentType("mod/@v/v1.0.0.mod"); got != "text/plain" {
		t.Errorf("mod content type %q", got)
	}
}
