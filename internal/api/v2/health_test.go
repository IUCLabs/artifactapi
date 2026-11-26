package v2

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/cache"
	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/testsupport"
)

func healthJSON(t *testing.T, h *HealthHandler) map[string]string {
	t.Helper()
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("health status code = %d", w.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	return body
}

func TestHealthS3State(t *testing.T) {
	if testDSN == "" {
		t.Skip("Docker unavailable")
	}
	ctx := context.Background()
	url, terminate, err := testsupport.StartRedis(ctx)
	if err != nil {
		t.Skipf("redis unavailable: %v", err)
	}
	t.Cleanup(terminate)

	db, err := database.New(testDSN)
	if err != nil {
		t.Fatalf("new db: %v", err)
	}
	t.Cleanup(db.Close)
	rc, err := cache.NewRedis(url)
	if err != nil {
		t.Fatalf("new redis: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	h := NewHealthHandler(db, rc, nil)

	cases := []struct {
		name       string
		set        func() error
		s3, status string
	}{
		{"missing key", func() error { return nil }, "unknown", "ok"},
		{"probe up", func() error { return rc.SetStoreHealth(ctx, true, time.Minute) }, "ok", "ok"},
		{"probe down", func() error { return rc.SetStoreHealth(ctx, false, time.Minute) }, "down", "degraded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.set(); err != nil {
				t.Fatal(err)
			}
			body := healthJSON(t, h)
			if body["s3"] != tc.s3 || body["status"] != tc.status {
				t.Errorf("s3=%q status=%q, want s3=%q status=%q", body["s3"], body["status"], tc.s3, tc.status)
			}
			if body["postgres"] != "ok" {
				t.Errorf("postgres = %q, want ok", body["postgres"])
			}
		})
	}

	t.Run("redis error", func(t *testing.T) {
		broken, err := cache.NewRedis(url)
		if err != nil {
			t.Fatal(err)
		}
		_ = broken.Close()
		body := healthJSON(t, NewHealthHandler(db, broken, nil))
		if body["s3"] != "unknown" || body["status"] != "ok" {
			t.Errorf("s3=%q status=%q, want unknown/ok", body["s3"], body["status"])
		}
	})
}
