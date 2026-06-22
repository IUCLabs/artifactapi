package models

import (
	"fmt"
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
	Description string      `json:"description,omitempty"`
	Username    string      `json:"-"`
	Password    string      `json:"-"`

	ImmutableTTL int  `json:"immutable_ttl"`
	MutableTTL   int  `json:"mutable_ttl"`
	CheckMutable bool `json:"check_mutable"`

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

type RemoteWithStats struct {
	Remote
	Stats RemoteStats `json:"stats"`
}
