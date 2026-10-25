package v2

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/database"
)

func TestVirtualParallelFetchRoundTrip(t *testing.T) {
	if testDSN == "" {
		t.Skip("Docker unavailable")
	}
	db, err := database.New(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := NewVirtualsHandler(db).Routes()
	parallel := func() bool {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/vpf", nil))
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 {
			t.Fatalf("get = %d %s", w.Code, w.Body)
		}
		v, ok := got["parallel_fetch"].(bool)
		if !ok {
			t.Fatalf("parallel_fetch missing from %s", w.Body)
		}
		return v
	}
	const body = `"package_type":"rpm","members":["a"]`

	steps := []struct {
		method, path, json string
		want               bool
	}{
		{"POST", "/", `{"name":"vpf",` + body + `}`, false},
		{"PUT", "/vpf", `{` + body + `,"parallel_fetch":true}`, true},
		{"PUT", "/vpf", `{` + body + `,"parallel_fetch":false}`, false},
	}
	for i, s := range steps {
		if c := do(t, h, s.method, s.path, s.json); c/100 != 2 {
			t.Fatalf("step %d %s %s = %d", i, s.method, s.path, c)
		}
		if got := parallel(); got != s.want {
			t.Errorf("step %d: parallel_fetch = %v, want %v", i, got, s.want)
		}
	}
	if c := do(t, h, "DELETE", "/vpf", ""); c != 204 {
		t.Fatalf("delete = %d", c)
	}
}
