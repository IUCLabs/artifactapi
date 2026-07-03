package auth

import (
	"encoding/base64"
	"testing"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

func TestBasicHeaders(t *testing.T) {
	h := BasicHeaders(models.Remote{Username: "alice", Password: "secret"})
	got := h.Get("Authorization")
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:secret"))
	if got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
}

func TestBasicHeadersNoUser(t *testing.T) {
	if h := BasicHeaders(models.Remote{}); h.Get("Authorization") != "" {
		t.Error("expected no Authorization header without a username")
	}
}
