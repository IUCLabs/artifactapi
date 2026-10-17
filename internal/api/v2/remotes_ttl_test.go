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
