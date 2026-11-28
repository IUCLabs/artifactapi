package deb

import (
	"context"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/githubauth/githubauthtest"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

type appTestCred string

func (s appTestCred) Token(context.Context) (string, error) { return string(s), nil }

func TestGitHubHeadersGitHubAppPrecedence(t *testing.T) {
	githubauthtest.InstallApp(t, "ci", "ghs_app")
	p := newTestProvider()
	p.serverCred = appTestCred("ghp_server")

	for _, c := range []struct {
		name   string
		remote models.Remote
		want   string
	}{
		{"app beats server", models.Remote{GitHubApp: "ci"}, "Bearer ghs_app"},
		{"server without app", models.Remote{}, "Bearer ghp_server"},
		{"remote token beats server", models.Remote{Password: "ghp_remote"}, "Bearer ghp_remote"},
	} {
		for _, api := range []bool{true, false} {
			h, err := p.githubHeaders(context.Background(), c.remote, api)
			if err != nil || h.Get("Authorization") != c.want {
				t.Errorf("%s (api=%v): Authorization = %q, %v; want %q", c.name, api, h.Get("Authorization"), err, c.want)
			}
		}
	}
	if _, err := p.githubHeaders(context.Background(), models.Remote{GitHubApp: "gone"}, true); err == nil {
		t.Error("missing github_app must fail rather than fall back to the server credential")
	}
}
