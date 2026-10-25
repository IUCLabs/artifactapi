package v2

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/database"
)

func TestCacheRepodataRoundTrip(t *testing.T) {
	if testDSN == "" {
		t.Skip("Docker unavailable")
	}
	db, err := database.New(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := NewRemotesHandler(db, nil, nil).Routes()
	get := func() bool {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/cache-rd", nil))
		var got struct {
			CacheRepodata *bool `json:"cache_repodata"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.CacheRepodata == nil {
			t.Fatalf("get = %d %s", w.Code, w.Body.String())
		}
		return *got.CacheRepodata
	}
	const body = `"package_type":"github_rpm","base_url":"https://api.github.com"`

	steps := []struct {
		method, path, json string
		want               bool
	}{
		{"POST", "/", `{"name":"cache-rd",` + body + `}`, false},
		{"PUT", "/cache-rd", `{` + body + `,"cache_repodata":true}`, true},
		{"PUT", "/cache-rd", `{` + body + `,"cache_repodata":false}`, false},
	}
	for i, s := range steps {
		if c := do(t, h, s.method, s.path, s.json); c/100 != 2 {
			t.Fatalf("step %d %s %s = %d", i, s.method, s.path, c)
		}
		if got := get(); got != s.want {
			t.Errorf("step %d: cache_repodata = %v, want %v", i, got, s.want)
		}
	}
	if c := do(t, h, "POST", "/", `{"name":"cache-rd-on",`+body+`,"cache_repodata":true}`); c != 201 {
		t.Fatalf("create with cache_repodata = %d", c)
	}
	if r, err := db.GetRemote(t.Context(), "cache-rd-on"); err != nil || !r.CacheRepodata {
		t.Fatalf("create did not store cache_repodata: %v %v", r, err)
	}
}
