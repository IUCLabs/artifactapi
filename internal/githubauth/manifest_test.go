package githubauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConvertManifest(t *testing.T) {
	keyPEM := testRSAKeyPEM(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "" {
			t.Errorf("conversion request %s auth=%q; want unauthenticated POST", r.Method, r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/api/v3/app-manifests/good/conversions":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "slug": "aa-ci", "pem": keyPEM,
				"webhook_secret": "whsec", "client_secret": "csec"})
		case "/api/v3/app-manifests/nokey/conversions":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "slug": "aa-ci", "pem": "junk"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	base := srv.URL + "/api/v3"

	conv, err := ConvertManifest(context.Background(), srv.Client(), base, "good")
	if err != nil || conv.ID != 42 || conv.Slug != "aa-ci" || conv.PEM != keyPEM {
		t.Fatalf("convert = %+v %v", conv, err)
	}
	_, err = ConvertManifest(context.Background(), srv.Client(), base, "expired")
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusNotFound {
		t.Fatalf("expired code err = %v; want StatusError 404", err)
	}
	if _, err := ConvertManifest(context.Background(), srv.Client(), base, "nokey"); err == nil || strings.Contains(err.Error(), "junk") {
		t.Fatalf("bad pem err = %v; want key error without key material", err)
	}
}

func TestInstallationAppIDSendsAppJWT(t *testing.T) {
	keyPEM := testRSAKeyPEM(t)
	key, _ := ParsePrivateKey(keyPEM)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		parts := strings.Split(tok, ".")
		payload, _ := base64.RawURLEncoding.DecodeString(parts[min(1, len(parts)-1)])
		var claims struct {
			Iss string `json:"iss"`
		}
		_ = json.Unmarshal(payload, &claims)
		if r.URL.Path != "/app/installations/9" || claims.Iss != "42" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 9, "app_id": 42})
	}))
	defer srv.Close()
	got, err := InstallationAppID(context.Background(), srv.Client(), srv.URL, 42, key, 9)
	if err != nil || got != 42 {
		t.Fatalf("InstallationAppID = %d %v", got, err)
	}
	var se *StatusError
	if _, err := InstallationAppID(context.Background(), srv.Client(), srv.URL, 42, key, 10); !errors.As(err, &se) || se.Status != 404 {
		t.Fatalf("foreign installation err = %v; want 404", err)
	}
}
