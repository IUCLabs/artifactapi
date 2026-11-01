package v2

import (
	"context"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/database"
)

// An omitted mutable_ttl defaults to 3600 on create and keeps the stored value
// on update; an explicit 0 is stored as 0 in both.
func TestMutableTTLOmittedVsZero(t *testing.T) {
	if testDSN == "" {
		t.Skip("Docker unavailable")
	}
	db, err := database.New(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := NewRemotesHandler(db, nil, nil).Routes()
	ttl := func(name string) int {
		t.Helper()
		r, err := db.GetRemote(context.Background(), name)
		if err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		return r.MutableTTL
	}
	const body = `"package_type":"rpm","base_url":"https://example.com/repo"`

	steps := []struct {
		method, path, json string
		want               int
	}{
		{"POST", "/", `{"name":"ttl-omit",` + body + `}`, 3600},
		{"PUT", "/ttl-omit", `{` + body + `,"mutable_ttl":120}`, 120},
		{"PUT", "/ttl-omit", `{` + body + `}`, 120},
		{"PUT", "/ttl-omit", `{` + body + `,"mutable_ttl":0}`, 0},
		{"PUT", "/ttl-omit", `{` + body + `}`, 0},
	}
	for i, s := range steps {
		if c := do(t, h, s.method, s.path, s.json); c/100 != 2 {
			t.Fatalf("step %d %s %s = %d", i, s.method, s.path, c)
		}
		if got := ttl("ttl-omit"); got != s.want {
			t.Errorf("step %d: mutable_ttl = %d, want %d", i, got, s.want)
		}
	}

	if c := do(t, h, "POST", "/", `{"name":"ttl-zero",`+body+`,"mutable_ttl":0}`); c != 201 {
		t.Fatalf("create explicit 0 = %d", c)
	}
	if got := ttl("ttl-zero"); got != 0 {
		t.Errorf("explicit 0 stored as %d", got)
	}
}

// sync_interval round-trips through the API and DB on github_* remotes and is
// rejected when negative, over the maximum, or on a type without a background syncer.
func TestSyncIntervalAPI(t *testing.T) {
	if testDSN == "" {
		t.Skip("Docker unavailable")
	}
	db, err := database.New(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := NewRemotesHandler(db, nil, nil).Routes()
	interval := func(name string) int {
		t.Helper()
		r, err := db.GetRemote(context.Background(), name)
		if err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		return r.SyncInterval
	}
	const gh = `"package_type":"github_deb","base_url":"https://api.github.com/repos/acme/tools","releases_remote":"github"`

	if c := do(t, h, "POST", "/", `{"name":"si-gh",`+gh+`}`); c != 201 {
		t.Fatalf("create unset = %d", c)
	}
	if got := interval("si-gh"); got != 0 {
		t.Fatalf("unset sync_interval stored as %d", got)
	}
	if c := do(t, h, "PUT", "/si-gh", `{`+gh+`,"sync_interval":3600}`); c != 200 {
		t.Fatalf("update = %d", c)
	}
	if got := interval("si-gh"); got != 3600 {
		t.Fatalf("sync_interval = %d, want 3600", got)
	}

	for _, body := range []string{
		`{"name":"si-neg",` + gh + `,"sync_interval":-1}`,
		`{"name":"si-max",` + gh + `,"sync_interval":2592001}`,
		`{"name":"si-int32",` + gh + `,"sync_interval":2147483648}`,
		`{"name":"si-rpm","package_type":"rpm","base_url":"https://example.com/repo","sync_interval":60}`,
	} {
		if c := do(t, h, "POST", "/", body); c != 400 {
			t.Errorf("%s = %d, want 400", body, c)
		}
	}
	if c := do(t, h, "PUT", "/si-gh", `{`+gh+`,"sync_interval":-1}`); c != 400 {
		t.Errorf("update negative = %d, want 400", c)
	}
	if got := interval("si-gh"); got != 3600 {
		t.Errorf("rejected update changed sync_interval to %d", got)
	}
}

func TestQuarantineDaysDefault(t *testing.T) {
	if testDSN == "" {
		t.Skip("Docker unavailable")
	}
	db, err := database.New(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := NewRemotesHandler(db, nil, nil).Routes()
	days := func() int {
		t.Helper()
		r, err := db.GetRemote(context.Background(), "q-days")
		if err != nil {
			t.Fatal(err)
		}
		return r.QuarantineDays
	}
	const body = `"package_type":"npm","base_url":"https://example.com","quarantine_enabled":true`
	for i, s := range []struct {
		method, path, json string
		want               int
	}{
		{"POST", "/", `{"name":"q-days",` + body + `}`, 3},
		{"PUT", "/q-days", `{` + body + `,"quarantine_days":14}`, 14},
		{"PUT", "/q-days", `{` + body + `}`, 14},
	} {
		if c := do(t, h, s.method, s.path, s.json); c/100 != 2 {
			t.Fatalf("step %d = %d", i, c)
		}
		if got := days(); got != s.want {
			t.Errorf("step %d: quarantine_days = %d, want %d", i, got, s.want)
		}
	}
}

func TestQuarantineDaysValidation(t *testing.T) {
	if testDSN == "" {
		t.Skip("Docker unavailable")
	}
	db, err := database.New(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := NewRemotesHandler(db, nil, nil).Routes()
	const body = `"package_type":"npm","base_url":"https://example.com"`
	for i, s := range []struct {
		method, path, json string
		want               int
	}{
		{"POST", "/", `{"name":"q-val",` + body + `,"quarantine_enabled":true,"quarantine_days":0}`, 400},
		{"POST", "/", `{"name":"q-val",` + body + `,"quarantine_enabled":true,"quarantine_days":-1}`, 400},
		{"POST", "/", `{"name":"q-val",` + body + `,"quarantine_days":0}`, 201},
		{"PUT", "/q-val", `{` + body + `,"quarantine_enabled":true}`, 400},
		{"PUT", "/q-val", `{` + body + `,"quarantine_enabled":true,"quarantine_days":1}`, 200},
		{"PUT", "/q-val", `{` + body + `,"quarantine_enabled":true,"quarantine_days":0}`, 400},
	} {
		if c := do(t, h, s.method, s.path, s.json); c != s.want {
			t.Errorf("step %d %s %s = %d, want %d", i, s.method, s.path, c, s.want)
		}
	}
}

func TestRepoTypeKeptOnUpdate(t *testing.T) {
	if testDSN == "" {
		t.Skip("Docker unavailable")
	}
	db, err := database.New(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := NewRemotesHandler(db, nil, nil).Routes()
	repoType := func(name string) string {
		t.Helper()
		r, err := db.GetRemote(context.Background(), name)
		if err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		return string(r.RepoType)
	}
	const body = `"package_type":"rpm","base_url":"https://example.com/repo"`
	for i, s := range []struct {
		method, path, json string
		code               int
		name, want         string
	}{
		{"POST", "/", `{"name":"rt-remote",` + body + `}`, 201, "rt-remote", "remote"},
		{"PUT", "/rt-remote", `{` + body + `}`, 200, "rt-remote", "remote"},
		{"POST", "/", `{"name":"rt-local","package_type":"rpm","repo_type":"local"}`, 201, "rt-local", "local"},
		{"PUT", "/rt-local", `{"package_type":"rpm","description":"x"}`, 200, "rt-local", "local"},
		{"PUT", "/rt-local", `{"package_type":"rpm","repo_type":""}`, 200, "rt-local", "local"},
		{"PUT", "/rt-local", `{"package_type":"rpm","repo_type":"bogus"}`, 400, "rt-local", "local"},
		{"PUT", "/rt-local", `{"package_type":"rpm","repo_type":"remote"}`, 400, "rt-local", "local"},
		{"PUT", "/rt-remote", `{"package_type":"rpm","repo_type":"local"}`, 200, "rt-remote", "local"},
	} {
		if c := do(t, h, s.method, s.path, s.json); c != s.code {
			t.Fatalf("step %d %s %s = %d, want %d", i, s.method, s.path, c, s.code)
		}
		if got := repoType(s.name); got != s.want {
			t.Errorf("step %d: repo_type = %q, want %q", i, got, s.want)
		}
	}
}
