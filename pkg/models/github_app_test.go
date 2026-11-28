package models

import "testing"

func TestGitHubAppSpecValidate(t *testing.T) {
	ok := GitHubAppSpec{AppID: 1, InstallationID: 2, PrivateKey: "pem"}
	if err := ok.Validate(nil); err != nil || ok.APIBaseURL != DefaultGitHubAPIBaseURL {
		t.Fatalf("valid spec: %v, api_base_url %q", err, ok.APIBaseURL)
	}
	ghes := GitHubAppSpec{AppID: 1, InstallationID: 2, PrivateKey: "pem", APIBaseURL: "https://ghe.example.com/api/v3/"}
	if err := ghes.Validate([]string{"ghe.example.com"}); err != nil || ghes.APIBaseURL != "https://ghe.example.com/api/v3" {
		t.Fatalf("allowlisted ghes spec: %v, %q", err, ghes.APIBaseURL)
	}
	explicit := GitHubAppSpec{AppID: 1, InstallationID: 2, PrivateKey: "pem", APIBaseURL: "https://API.github.com/"}
	if err := explicit.Validate(nil); err != nil {
		t.Fatalf("explicit api.github.com: %v", err)
	}
	for name, s := range map[string]GitHubAppSpec{
		"no app_id":          {InstallationID: 2, PrivateKey: "pem"},
		"negative app_id":    {AppID: -1, InstallationID: 2, PrivateKey: "pem"},
		"no installation_id": {AppID: 1, PrivateKey: "pem"},
		"no private_key":     {AppID: 1, InstallationID: 2, PrivateKey: "  "},
		"http base":          {AppID: 1, InstallationID: 2, PrivateKey: "pem", APIBaseURL: "http://api.github.com"},
		"relative base":      {AppID: 1, InstallationID: 2, PrivateKey: "pem", APIBaseURL: "/api"},
		"unlisted host":      {AppID: 1, InstallationID: 2, PrivateKey: "pem", APIBaseURL: "https://evil.example.com"},
		"lookalike host":     {AppID: 1, InstallationID: 2, PrivateKey: "pem", APIBaseURL: "https://api.github.com.evil.example.com"},
		"userinfo":           {AppID: 1, InstallationID: 2, PrivateKey: "pem", APIBaseURL: "https://x@ghe.example.com"},
	} {
		if err := s.Validate([]string{"ghe.example.com"}); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestValidateGitHubAPIBaseURL(t *testing.T) {
	allowed := []string{"ghe.example.com", "GHE2.example.com:8443"}
	for _, u := range []string{
		"https://api.github.com",
		"https://API.GitHub.com/",
		"https://api.github.com:443",
		"https://ghe.example.com/api/v3",
		"https://GHE.example.com:443/api/v3/",
		"https://ghe2.example.com:8443/api/v3",
	} {
		if err := ValidateGitHubAPIBaseURL(u, allowed); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	for name, u := range map[string]string{
		"http":                 "http://api.github.com",
		"relative":             "/api",
		"github on other port": "https://api.github.com:8443",
		"bare host other port": "https://ghe.example.com:8443",
		"host:port wrong port": "https://ghe2.example.com:9443",
		"host:port on default": "https://ghe2.example.com",
		"unlisted host":        "https://evil.example.com",
		"lookalike host":       "https://api.github.com.evil.example.com",
		"trailing dot host":    "https://api.github.com.",
		"trailing dot ghes":    "https://ghe.example.com./api/v3",
		"empty port":           "https://api.github.com:",
		"userinfo":             "https://x@ghe.example.com",
		"query":                "https://ghe.example.com/api/v3?x=1",
		"empty query":          "https://ghe.example.com/api/v3?",
		"fragment":             "https://ghe.example.com/api/v3#x",
		"empty fragment":       "https://ghe.example.com/api/v3#",
		"dotdot":               "https://ghe.example.com/api/../v3",
		"trailing dotdot":      "https://ghe.example.com/api/..",
		"encoded dotdot":       "https://ghe.example.com/api/%2e%2e/v3",
		"dot segment":          "https://ghe.example.com/./api",
		"space in path":        "https://ghe.example.com/api v3",
		"encoded space":        "https://ghe.example.com/api%20v3",
		"tab":                  "https://ghe.example.com/api\tv3",
		"leading space":        " https://api.github.com",
		"newline":              "https://api.github.com\n",
	} {
		if err := ValidateGitHubAPIBaseURL(u, allowed); err == nil {
			t.Errorf("%s (%q): want error", name, u)
		}
	}
}

func TestParseGitHubAPIHost(t *testing.T) {
	for in, want := range map[string]string{
		"ghe.example.com":      "ghe.example.com",
		" GHE.Example.com ":    "ghe.example.com",
		"ghe.example.com:8443": "ghe.example.com:8443",
		"ghe.example.com:443":  "ghe.example.com",
		"localhost":            "localhost",
	} {
		if got, err := ParseGitHubAPIHost(in); err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", "https://ghe.example.com", "ghe.example.com/api", "ghe.example.com.", "ghe..example.com",
		"ghe.example.com:", "ghe.example.com:0", "ghe.example.com:65536", "ghe.example.com:08443",
		"ghe.example.com:x", "user@ghe.example.com", "-ghe.example.com", "ghe_x.example.com", "[::1]:443",
		"ghe example.com",
	} {
		if got, err := ParseGitHubAPIHost(in); err == nil {
			t.Errorf("%q: got %q; want error", in, got)
		}
	}
}

func TestRemoteValidateGitHubApp(t *testing.T) {
	remote := func(pt PackageType, base string) Remote {
		return Remote{PackageType: pt, RepoType: RepoTypeRemote, BaseURL: base, GitHubApp: "ci"}
	}
	for _, r := range []Remote{
		remote(PackageGitHubRPM, "https://api.github.com/repos/a/b"),
		remote(PackageGitHubDeb, "https://api.github.com/repos/a/b"),
		remote(PackageGitHubAlpine, "https://api.github.com/repos/a/b"),
		remote(PackageGeneric, "https://github.com"),
		remote(PackageVCS, "https://github.com"),
		{PackageType: PackageRPM},
	} {
		if err := r.ValidateGitHubApp(); err != nil {
			t.Errorf("%s %s: %v", r.PackageType, r.BaseURL, err)
		}
	}
	withCreds := remote(PackageGitHubRPM, "https://api.github.com/repos/a/b")
	withCreds.Password = "tok"
	local := remote(PackageGeneric, "https://github.com")
	local.RepoType = RepoTypeLocal
	for name, r := range map[string]Remote{
		"rpm":                remote(PackageRPM, "https://mirror.example.com"),
		"generic non-github": remote(PackageGeneric, "https://example.com"),
		"vcs gitea":          remote(PackageVCS, "https://gitea.example.com"),
		"local":              local,
		"with password":      withCreds,
	} {
		if err := r.ValidateGitHubApp(); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}
