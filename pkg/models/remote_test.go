package models

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
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

func TestSyncPeriod(t *testing.T) {
	const def = 5 * time.Minute
	for _, tc := range []struct {
		interval, ttl int
		want          time.Duration
	}{
		{0, 0, def},
		{0, 600, 10 * time.Minute},
		{3600, 600, time.Hour},
	} {
		r := Remote{SyncInterval: tc.interval, MutableTTL: tc.ttl}
		if got := r.SyncPeriod(def); got != tc.want {
			t.Errorf("SyncPeriod(interval=%d, ttl=%d) = %v, want %v", tc.interval, tc.ttl, got, tc.want)
		}
	}
}

func TestValidateSyncInterval(t *testing.T) {
	for _, tc := range []struct {
		pt       PackageType
		interval int
		ok       bool
	}{
		{PackageGitHubRPM, 3600, true},
		{PackageGitHubDeb, 3600, true},
		{PackageGitHubAlpine, 3600, true},
		{PackageGitHubRPM, -1, false},
		{PackageGitHubRPM, MaxSyncInterval, true},
		{PackageGitHubRPM, MaxSyncInterval + 1, false},
		{PackageRPM, 0, true},
		{PackageRPM, 60, false},
	} {
		r := Remote{PackageType: tc.pt, SyncInterval: tc.interval}
		if err := r.ValidateSyncInterval(); (err == nil) != tc.ok {
			t.Errorf("%s interval=%d: err=%v, want ok=%v", tc.pt, tc.interval, err, tc.ok)
		}
	}
}

func TestSyncSchedule(t *testing.T) {
	if i, p := (Remote{Name: "x", MutableTTL: 60}).SyncSchedule(); i != 0 || p != 0 {
		t.Fatalf("unset sync_interval schedule = %v/%v, want 0/0", i, p)
	}

	const period = 5 * time.Minute
	phases := map[time.Duration]bool{}
	var lo, hi time.Duration = period, 0
	for i := 0; i < 13; i++ {
		r := Remote{Name: fmt.Sprintf("github-rpm-%02d", i), SyncInterval: int(period.Seconds())}
		interval, p := r.SyncSchedule()
		if interval != period || p < 0 || p >= period {
			t.Fatalf("%s: schedule %v/%v outside [0, %v)", r.Name, interval, p, period)
		}
		if _, again := r.SyncSchedule(); again != p {
			t.Fatalf("%s: phase not stable", r.Name)
		}
		p = p.Truncate(time.Second)
		phases[p] = true
		lo, hi = min(lo, p), max(hi, p)
	}
	if len(phases) != 13 || hi-lo < period/2 {
		t.Fatalf("phases bunched: %d distinct, spread %v of %v", len(phases), hi-lo, period)
	}
}
