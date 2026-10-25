package models

import (
	"fmt"
	"net/url"
	"regexp"
	"time"
)

type RepoType string

const (
	RepoTypeRemote RepoType = "remote"
	RepoTypeLocal  RepoType = "local"
)

var validRepoTypes = map[RepoType]bool{
	RepoTypeRemote: true,
	RepoTypeLocal:  true,
}

func (r RepoType) Valid() bool {
	return validRepoTypes[r]
}

func (r RepoType) String() string {
	return string(r)
}

func ParseRepoType(s string) (RepoType, error) {
	rt := RepoType(s)
	if !rt.Valid() {
		return "", fmt.Errorf("unknown repo type: %q", s)
	}
	return rt, nil
}

type Remote struct {
	Name        string      `json:"name"`
	PackageType PackageType `json:"package_type"`
	RepoType    RepoType    `json:"repo_type"`
	BaseURL     string      `json:"base_url"`
	// Mirrorlist holds additional upstream mirror base URLs. The effective
	// upstream pool is [base_url] + mirrorlist, load-balanced round-robin with
	// failover by the proxy engine. Only valid on remote rpm/deb/apk repos.
	Mirrorlist []string `json:"mirrorlist,omitempty"`
	// MirrorStrategy selects how the proxy engine picks the starting upstream
	// from the pool: round_robin (default/empty) rotates, least_conn favors the
	// mirror with the fewest in-flight requests. Failover order is unchanged.
	MirrorStrategy string `json:"mirror_strategy,omitempty"`
	Description    string `json:"description,omitempty"`
	Username       string `json:"-"`
	Password       string `json:"-"`

	ImmutableTTL int  `json:"immutable_ttl"`
	MutableTTL   int  `json:"mutable_ttl"`
	CheckMutable bool `json:"check_mutable"`

	// Upstream HTTP timeouts in seconds. 0 means use the server default.
	UpstreamDialTimeout           int `json:"upstream_dial_timeout,omitempty"`
	UpstreamTLSTimeout            int `json:"upstream_tls_timeout,omitempty"`
	UpstreamResponseHeaderTimeout int `json:"upstream_response_header_timeout,omitempty"`

	Patterns          []string `json:"patterns,omitempty"`
	Blocklist         []string `json:"blocklist,omitempty"`
	MutablePatterns   []string `json:"mutable_patterns,omitempty"`
	ImmutablePatterns []string `json:"immutable_patterns,omitempty"`

	BanTagsEnabled bool     `json:"ban_tags_enabled,omitempty"`
	BanTags        []string `json:"ban_tags,omitempty"`

	QuarantineEnabled bool `json:"quarantine_enabled,omitempty"`
	QuarantineDays    int  `json:"quarantine_days,omitempty"`

	StaleOnError bool `json:"stale_on_error"`

	ReleasesRemote string `json:"releases_remote,omitempty"`
	ManagedBy      string `json:"managed_by,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Mirror balancing strategies for MirrorStrategy. An empty value is treated as
// round_robin, so existing remotes keep their current behavior.
const (
	MirrorStrategyRoundRobin = "round_robin"
	MirrorStrategyLeastConn  = "least_conn"
)

// mirrorlistPackageTypes are the package types for which a mirrorlist is
// allowed: OS package repos (rpm, deb, apk/alpine) that fetch many small files
// and benefit most from mirror load-balancing and failover.
var mirrorlistPackageTypes = map[PackageType]bool{
	PackageRPM:    true,
	PackageDeb:    true,
	PackageAlpine: true,
}

// UpstreamPool returns the ordered upstream base URLs for this remote: the
// primary base_url first, followed by any mirrorlist entries. The proxy engine
// load-balances round-robin across the pool and fails over between them.
func (r Remote) UpstreamPool() []string {
	pool := make([]string, 0, 1+len(r.Mirrorlist))
	if r.BaseURL != "" {
		pool = append(pool, r.BaseURL)
	}
	pool = append(pool, r.Mirrorlist...)
	return pool
}

// ValidateMirrorlist enforces that a mirrorlist is only configured on remote
// rpm/deb/apk repositories and that every entry is a parseable http/https URL.
func (r *Remote) ValidateMirrorlist() error {
	if len(r.Mirrorlist) == 0 {
		return nil
	}
	if r.RepoType != RepoTypeRemote {
		return fmt.Errorf("mirrorlist is only allowed on remote repositories")
	}
	if !mirrorlistPackageTypes[r.PackageType] {
		return fmt.Errorf("mirrorlist is only allowed for rpm, deb and alpine package types, not %q", r.PackageType)
	}
	for _, u := range r.Mirrorlist {
		parsed, err := url.ParseRequestURI(u)
		if err != nil {
			return fmt.Errorf("invalid mirrorlist url %q: %w", u, err)
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return fmt.Errorf("mirrorlist url %q must be http or https", u)
		}
	}
	return nil
}

// ValidateMirrorStrategy enforces that mirror_strategy is one of the allowed
// values and that a non-default strategy (least_conn) is only set alongside a
// non-empty mirrorlist, where balancing is meaningful. An empty strategy is
// accepted and behaves as round_robin.
func (r *Remote) ValidateMirrorStrategy() error {
	switch r.MirrorStrategy {
	case "", MirrorStrategyRoundRobin, MirrorStrategyLeastConn:
	default:
		return fmt.Errorf("invalid mirror_strategy %q: must be %q or %q", r.MirrorStrategy, MirrorStrategyRoundRobin, MirrorStrategyLeastConn)
	}
	if r.MirrorStrategy == MirrorStrategyLeastConn && len(r.Mirrorlist) == 0 {
		return fmt.Errorf("mirror_strategy %q requires a non-empty mirrorlist", r.MirrorStrategy)
	}
	return nil
}

// DefaultQuarantineDays matches the remotes.quarantine_days column default.
const DefaultQuarantineDays = 3

// ValidateQuarantine rejects an enabled quarantine with no window.
func (r *Remote) ValidateQuarantine() error {
	if r.QuarantineEnabled && r.QuarantineDays < 1 {
		return fmt.Errorf("quarantine_days must be at least 1 when quarantine_enabled is set")
	}
	return nil
}

// ValidatePatterns ensures every configured regex compiles. Storing an
// invalid pattern would otherwise be silently dropped at match time, which
// for the blocklist is a fail-open: a mistyped deny rule becomes a no-op.
func (r *Remote) ValidatePatterns() error {
	groups := []struct {
		field    string
		patterns []string
	}{
		{"patterns", r.Patterns},
		{"blocklist", r.Blocklist},
		{"mutable_patterns", r.MutablePatterns},
		{"immutable_patterns", r.ImmutablePatterns},
		{"ban_tags", r.BanTags},
	}
	for _, g := range groups {
		for _, p := range g.patterns {
			if _, err := regexp.Compile(p); err != nil {
				return fmt.Errorf("invalid regex in %s: %q: %w", g.field, p, err)
			}
		}
	}
	return nil
}

type RemoteWithStats struct {
	Remote
	Stats RemoteStats `json:"stats"`
}
