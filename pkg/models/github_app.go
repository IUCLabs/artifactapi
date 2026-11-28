package models

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// DefaultGitHubAPIBaseURL is the api_base_url a GitHub App gets when none is set.
const DefaultGitHubAPIBaseURL = "https://api.github.com"

// ErrGitHubAppNotFound is returned when no GitHub App has the requested name.
var ErrGitHubAppNotFound = errors.New("github app not found")

// GitHubApp is an API-managed GitHub App credential that remotes reference by
// name. PrivateKey is write-only: it is never serialised.
type GitHubApp struct {
	Name                  string    `json:"name"`
	AppID                 int64     `json:"app_id"`
	InstallationID        int64     `json:"installation_id"`
	APIBaseURL            string    `json:"api_base_url"`
	Description           string    `json:"description"`
	PrivateKeyFingerprint string    `json:"private_key_fingerprint"`
	PrivateKey            string    `json:"-"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

// GitHubAppSpec is the PUT body for a GitHub App.
type GitHubAppSpec struct {
	AppID          int64  `json:"app_id"`
	InstallationID int64  `json:"installation_id"`
	PrivateKey     string `json:"private_key"`
	APIBaseURL     string `json:"api_base_url"`
	Description    string `json:"description"`
}

// Validate checks the spec's scalar fields and defaults api_base_url, which
// must pass ValidateGitHubAPIBaseURL. The private key is parsed by the caller.
func (s *GitHubAppSpec) Validate(allowedHosts []string) error {
	if s.AppID <= 0 {
		return errors.New("app_id is required and must be > 0")
	}
	if s.InstallationID <= 0 {
		return errors.New("installation_id is required and must be > 0")
	}
	if strings.TrimSpace(s.PrivateKey) == "" {
		return errors.New("private_key is required")
	}
	if s.APIBaseURL == "" {
		s.APIBaseURL = DefaultGitHubAPIBaseURL
	}
	if err := ValidateGitHubAPIBaseURL(s.APIBaseURL, allowedHosts); err != nil {
		return err
	}
	s.APIBaseURL = strings.TrimRight(s.APIBaseURL, "/")
	return nil
}

// ValidateGitHubAPIBaseURL checks rawURL is a plain https base URL (no
// userinfo, query, fragment, whitespace or dot segments) whose host:port is
// api.github.com on the default port or matches an allowedHosts entry: a bare
// host matches the default port only, host:port matches exactly.
func ValidateGitHubAPIBaseURL(rawURL string, allowedHosts []string) error {
	bad := fmt.Errorf("api_base_url must be a plain absolute https URL, got %q", rawURL)
	if strings.ContainsFunc(rawURL, unicode.IsSpace) {
		return bad
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Opaque != "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(rawURL, "#") {
		return bad
	}
	if strings.ContainsFunc(u.Path, unicode.IsSpace) {
		return bad
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return bad
		}
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if host == "" || strings.HasSuffix(host, ".") || strings.HasSuffix(u.Host, ":") {
		return bad
	}
	if port == "443" {
		port = ""
	}
	target := host
	if port != "" {
		target = net.JoinHostPort(host, port)
	}
	if target == "api.github.com" || slices.ContainsFunc(allowedHosts, func(e string) bool {
		return strings.TrimSuffix(strings.ToLower(e), ":443") == target
	}) {
		return nil
	}
	return fmt.Errorf("api_base_url host %q is not api.github.com or listed in GITHUB_APP_API_HOSTS", target)
}

var apiHostLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// ParseGitHubAPIHost validates a GITHUB_APP_API_HOSTS entry, a bare hostname or
// host:port, and returns it lowercased.
func ParseGitHubAPIHost(entry string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(entry))
	bad := fmt.Errorf("invalid GitHub API host %q: want a bare hostname or host:port", entry)
	host := h
	if strings.Contains(h, ":") {
		var port string
		var err error
		if host, port, err = net.SplitHostPort(h); err != nil {
			return "", bad
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return "", bad
		}
		if port == "443" {
			h = host
		}
	}
	if host == "" || len(host) > 253 {
		return "", bad
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) > 63 || !apiHostLabelRe.MatchString(label) {
			return "", bad
		}
	}
	return h, nil
}

// IsGitHubHost reports whether rawURL targets a GitHub API/download host that
// accepts a GitHub credential. objects.githubusercontent.com is deliberately
// excluded: release-asset downloads 302-redirect there with a pre-signed URL
// that must not carry an Authorization header.
func IsGitHubHost(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "github.com", "www.github.com", "api.github.com", "codeload.github.com", "uploads.github.com":
		return true
	}
	return false
}

// UsesGitHub reports whether the remote makes GitHub requests that a GitHub
// credential authenticates.
func (r *Remote) UsesGitHub() bool {
	if r.RepoType != RepoTypeRemote {
		return false
	}
	switch r.PackageType {
	case PackageGitHubRPM, PackageGitHubDeb, PackageGitHubAlpine:
		return true
	case PackageGeneric, PackageVCS:
		return IsGitHubHost(r.BaseURL)
	}
	return false
}

// ValidateGitHubApp checks github_app is only set on a remote that calls
// GitHub and never alongside the remote's own username/password.
func (r *Remote) ValidateGitHubApp() error {
	if r.GitHubApp == "" {
		return nil
	}
	if r.Username != "" || r.Password != "" {
		return errors.New("github_app cannot be combined with a remote username/password")
	}
	if !r.UsesGitHub() {
		return fmt.Errorf("github_app is only valid on remotes that call GitHub (github_rpm, github_deb, github_alpine, or generic/vcs with a GitHub base_url), not %s %q", r.RepoType, r.PackageType)
	}
	return nil
}
