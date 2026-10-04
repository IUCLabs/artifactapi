package v2

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/IUCLabs/artifactapi/internal/proxy"
)

type fakeEvictor struct {
	remote, path string
	err          error
}

func (f *fakeEvictor) Evict(_ context.Context, remote, path string) error {
	f.remote, f.path = remote, path
	return f.err
}

func deleteObject(ev Evictor, path string) int {
	router := chi.NewRouter()
	router.Route("/remotes/{name}/objects", func(r chi.Router) {
		r.Delete("/*", NewObjectsHandler(nil).Routes(ev).ServeHTTP)
	})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("DELETE", "/remotes/epel/objects/"+path, nil))
	return w.Code
}

func TestRemoteEvictDelegatesToEvictor(t *testing.T) {
	for _, path := range []string{"8/Everything/x86_64/repodata/repomd.xml", "8/Everything/x86_64/repodata/*"} {
		ev := &fakeEvictor{}
		if code := deleteObject(ev, path); code != 204 || ev.remote != "epel" || ev.path != path {
			t.Errorf("DELETE %s: code=%d evicted=%q/%q", path, code, ev.remote, ev.path)
		}
	}
}

func TestRemoteEvictMapsErrorStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"bad wildcard":   {&proxy.ProxyError{Status: http.StatusBadRequest, Message: "wildcard evict must be <dir>/*"}, http.StatusBadRequest},
		"unknown remote": {&proxy.ProxyError{Status: http.StatusNotFound, Message: "remote not found"}, http.StatusNotFound},
		"lock busy":      {fmt.Errorf("wrapped: %w", &proxy.ProxyError{Status: http.StatusServiceUnavailable, Message: "retry"}), http.StatusServiceUnavailable},
		"other error":    {errors.New("db down"), http.StatusInternalServerError},
	} {
		if code := deleteObject(&fakeEvictor{err: tc.err}, "x"); code != tc.want {
			t.Errorf("%s: code = %d, want %d", name, code, tc.want)
		}
	}
}
