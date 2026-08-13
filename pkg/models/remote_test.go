package models

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRemote_ValidatePatterns(t *testing.T) {
	valid := &Remote{
		Patterns:          []string{`.*\.tar\.gz$`},
		Blocklist:         []string{`^secret/`},
		ImmutablePatterns: []string{`\.rpm$`},
	}
	if err := valid.ValidatePatterns(); err != nil {
		t.Fatalf("expected valid patterns, got %v", err)
	}

	bad := &Remote{Blocklist: []string{`[unterminated`}}
	if err := bad.ValidatePatterns(); err == nil {
		t.Fatal("expected error for invalid blocklist regex, got nil")
	}
}

func TestRemoteMirrorlistJSON(t *testing.T) {
	// base_url stays a plain string; mirrorlist round-trips as an array.
	var r Remote
	body := `{"name":"x","package_type":"rpm","repo_type":"remote","base_url":"https://a.example","mirrorlist":["https://b.example","https://c.example"]}`
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatal(err)
	}
	if r.BaseURL != "https://a.example" {
		t.Errorf("BaseURL = %q, want https://a.example", r.BaseURL)
	}
	if len(r.Mirrorlist) != 2 || r.Mirrorlist[0] != "https://b.example" || r.Mirrorlist[1] != "https://c.example" {
		t.Errorf("Mirrorlist = %v, want two entries", r.Mirrorlist)
	}

	out, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"base_url":"https://a.example"`) {
		t.Errorf("marshal lost base_url: %s", out)
	}
	if !strings.Contains(string(out), `"mirrorlist":["https://b.example","https://c.example"]`) {
		t.Errorf("marshal lost mirrorlist: %s", out)
	}
}

func TestRemoteMirrorlistOmitempty(t *testing.T) {
	out, err := json.Marshal(Remote{Name: "x", PackageType: PackageRPM, RepoType: RepoTypeRemote, BaseURL: "https://a.example"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "mirrorlist") {
		t.Errorf("empty mirrorlist should be omitted: %s", out)
	}
}

func TestUpstreamPool(t *testing.T) {
	// base_url first, then mirrorlist.
	r := Remote{BaseURL: "https://a.example", Mirrorlist: []string{"https://b.example", "https://c.example"}}
	pool := r.UpstreamPool()
	want := []string{"https://a.example", "https://b.example", "https://c.example"}
	if strings.Join(pool, ",") != strings.Join(want, ",") {
		t.Errorf("UpstreamPool = %v, want %v", pool, want)
	}

	// No mirrorlist ⇒ pool is just [base_url].
	solo := Remote{BaseURL: "https://a.example"}
	if got := solo.UpstreamPool(); len(got) != 1 || got[0] != "https://a.example" {
		t.Errorf("solo UpstreamPool = %v, want [base_url]", got)
	}
}

func TestValidateMirrorStrategy(t *testing.T) {
	ml := []string{"https://m.example"}
	cases := []struct {
		name    string
		remote  Remote
		wantErr bool
	}{
		{"empty defaults ok", Remote{Mirrorlist: ml}, false},
		{"explicit round_robin ok", Remote{MirrorStrategy: MirrorStrategyRoundRobin, Mirrorlist: ml}, false},
		{"round_robin without mirrorlist ok", Remote{MirrorStrategy: MirrorStrategyRoundRobin}, false},
		{"least_conn with mirrorlist ok", Remote{MirrorStrategy: MirrorStrategyLeastConn, Mirrorlist: ml}, false},
		{"least_conn without mirrorlist rejected", Remote{MirrorStrategy: MirrorStrategyLeastConn}, true},
		{"unknown strategy rejected", Remote{MirrorStrategy: "random", Mirrorlist: ml}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.remote.ValidateMirrorStrategy()
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateMirrorStrategy() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateMirrorlist(t *testing.T) {
	cases := []struct {
		name    string
		remote  Remote
		wantErr bool
	}{
		{"empty is ok on anything", Remote{RepoType: RepoTypeRemote, PackageType: PackageGeneric}, false},
		{"rpm remote ok", Remote{RepoType: RepoTypeRemote, PackageType: PackageRPM, Mirrorlist: []string{"https://m.example"}}, false},
		{"deb remote ok", Remote{RepoType: RepoTypeRemote, PackageType: PackageDeb, Mirrorlist: []string{"http://m.example"}}, false},
		{"alpine remote ok", Remote{RepoType: RepoTypeRemote, PackageType: PackageAlpine, Mirrorlist: []string{"https://m.example"}}, false},
		{"generic remote rejected", Remote{RepoType: RepoTypeRemote, PackageType: PackageGeneric, Mirrorlist: []string{"https://m.example"}}, true},
		{"docker remote rejected", Remote{RepoType: RepoTypeRemote, PackageType: PackageDocker, Mirrorlist: []string{"https://m.example"}}, true},
		{"local rpm rejected", Remote{RepoType: RepoTypeLocal, PackageType: PackageRPM, Mirrorlist: []string{"https://m.example"}}, true},
		{"bad scheme rejected", Remote{RepoType: RepoTypeRemote, PackageType: PackageRPM, Mirrorlist: []string{"ftp://m.example"}}, true},
		{"unparseable rejected", Remote{RepoType: RepoTypeRemote, PackageType: PackageRPM, Mirrorlist: []string{"://nope"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.remote.ValidateMirrorlist()
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateMirrorlist() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
