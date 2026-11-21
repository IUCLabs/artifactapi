package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

func testServer(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL)
}

func TestRemotesRoundTrip(t *testing.T) {
	var gotMethod, gotPath string
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/remotes":
			_, _ = w.Write([]byte(`[{"name":"a"},{"name":"b"}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/remotes/a":
			_, _ = w.Write([]byte(`{"name":"a"}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"name":"a"}`))
		}
	})
	ctx := context.Background()

	remotes, err := c.ListRemotes(ctx)
	if err != nil || len(remotes) != 2 {
		t.Fatalf("ListRemotes: %v %v", remotes, err)
	}
	if r, err := c.GetRemote(ctx, "a"); err != nil || r.Name != "a" {
		t.Fatalf("GetRemote: %v %v", r, err)
	}
	if err := c.CreateRemote(ctx, &models.Remote{Name: "a", PackageType: models.PackageGeneric}); err != nil {
		t.Fatalf("CreateRemote: %v", err)
	}
	if err := c.UpdateRemote(ctx, &models.Remote{Name: "a"}); err != nil {
		t.Fatalf("UpdateRemote: %v", err)
	}
	if err := c.DeleteRemote(ctx, "a"); err != nil {
		t.Fatalf("DeleteRemote: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/v2/remotes/a" {
		t.Errorf("last call = %s %s", gotMethod, gotPath)
	}
}

func TestVirtualsRoundTrip(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/virtuals"):
			_, _ = w.Write([]byte(`[{"name":"v"}]`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"name":"v"}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"name":"v"}`))
		}
	})
	ctx := context.Background()
	if vs, err := c.ListVirtuals(ctx); err != nil || len(vs) != 1 {
		t.Fatalf("ListVirtuals: %v %v", vs, err)
	}
	if v, err := c.GetVirtual(ctx, "v"); err != nil || v.Name != "v" {
		t.Fatalf("GetVirtual: %v %v", v, err)
	}
	if err := c.CreateVirtual(ctx, &models.Virtual{Name: "v"}); err != nil {
		t.Fatalf("CreateVirtual: %v", err)
	}
	if err := c.UpdateVirtual(ctx, &models.Virtual{Name: "v"}); err != nil {
		t.Fatalf("UpdateVirtual: %v", err)
	}
	if err := c.DeleteVirtual(ctx, "v"); err != nil {
		t.Fatalf("DeleteVirtual: %v", err)
	}
}

func TestStatsHealthObjects(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/stats"):
			_, _ = w.Write([]byte(`{"total_remotes":3}`))
		case strings.HasSuffix(r.URL.Path, "/health"):
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			_, _ = w.Write([]byte(`[{"path":"p"}]`))
		}
	})
	ctx := context.Background()
	if _, err := c.Stats(ctx); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if _, err := c.Health(ctx); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if objs, err := c.ListObjects(ctx, "r", 1, 50); err != nil || len(objs) != 1 {
		t.Fatalf("ListObjects: %v %v", objs, err)
	}
	if err := c.EvictObject(ctx, "r", "some/path"); err != nil {
		t.Fatalf("EvictObject: %v", err)
	}
}

func TestErrorResponses(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	ctx := context.Background()
	_, err := c.GetRemote(ctx, "x")
	if err == nil || !strings.Contains(err.Error(), "api error 500") {
		t.Errorf("expected api error, got %v", err)
	}
}

func TestDecodeError(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	})
	if _, err := c.ListRemotes(context.Background()); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Errorf("expected decode error, got %v", err)
	}
}

func TestRequestError(t *testing.T) {
	// Invalid base URL triggers request construction failure.
	c := New("http://[::1]:namedport")
	if err := c.DeleteRemote(context.Background(), "x"); err == nil {
		t.Error("expected request error for invalid URL")
	}
}
