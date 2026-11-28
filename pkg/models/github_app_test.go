package models

import "testing"

func TestGitHubAppSpecValidate(t *testing.T) {
	ok := GitHubAppSpec{AppID: 1, InstallationID: 2, PrivateKey: "pem"}
	if err := ok.Validate(); err != nil || ok.APIBaseURL != DefaultGitHubAPIBaseURL {
		t.Fatalf("valid spec: %v, api_base_url %q", err, ok.APIBaseURL)
	}
	ghes := GitHubAppSpec{AppID: 1, InstallationID: 2, PrivateKey: "pem", APIBaseURL: "https://ghe.example.com/api/v3/"}
	if err := ghes.Validate(); err != nil || ghes.APIBaseURL != "https://ghe.example.com/api/v3" {
		t.Fatalf("ghes spec: %v, %q", err, ghes.APIBaseURL)
	}
	for name, s := range map[string]GitHubAppSpec{
		"no app_id":          {InstallationID: 2, PrivateKey: "pem"},
		"negative app_id":    {AppID: -1, InstallationID: 2, PrivateKey: "pem"},
		"no installation_id": {AppID: 1, PrivateKey: "pem"},
		"no private_key":     {AppID: 1, InstallationID: 2, PrivateKey: "  "},
		"http base":          {AppID: 1, InstallationID: 2, PrivateKey: "pem", APIBaseURL: "http://api.github.com"},
		"relative base":      {AppID: 1, InstallationID: 2, PrivateKey: "pem", APIBaseURL: "/api"},
	} {
		if err := s.Validate(); err == nil {
			t.Errorf("%s: want error", name)
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
