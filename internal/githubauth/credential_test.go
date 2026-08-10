package githubauth

import (
	"context"
	"testing"
)

func TestNew_NoConfigIsAnonymous(t *testing.T) {
	c, err := New(Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c != nil {
		t.Fatalf("expected nil credential when nothing configured, got %T", c)
	}
}

func TestNew_TokenMode(t *testing.T) {
	c, err := New(Options{Token: "ghp_example"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tok, err := c.Token(context.Background())
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if tok != "ghp_example" {
		t.Fatalf("token = %q, want ghp_example", tok)
	}
}

func TestNew_TokenAndAppConflict(t *testing.T) {
	_, err := New(Options{Token: "ghp_example", AppID: "123"})
	if err == nil {
		t.Fatal("expected error when both token and app fields are set")
	}
}

func TestNew_PartialAppFailsClosed(t *testing.T) {
	cases := map[string]Options{
		"app id without key":  {AppID: "123", InstallationID: "456"},
		"key without app id":  {InstallationID: "456", PrivateKeyPEM: testRSAKeyPEM(t)},
		"app id without inst": {AppID: "123", PrivateKeyPEM: testRSAKeyPEM(t)},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(opts); err == nil {
				t.Fatalf("expected fail-closed error for %q", name)
			}
		})
	}
}

func TestNew_AppModeParsesKey(t *testing.T) {
	c, err := New(Options{
		AppID:          "123",
		InstallationID: "456",
		PrivateKeyPEM:  testRSAKeyPEM(t),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := c.(*appCredential); !ok {
		t.Fatalf("expected *appCredential, got %T", c)
	}
}

func TestNew_AppModeRejectsBadKey(t *testing.T) {
	_, err := New(Options{
		AppID:          "123",
		InstallationID: "456",
		PrivateKeyPEM:  "-----BEGIN RSA PRIVATE KEY-----\nnope\n-----END RSA PRIVATE KEY-----",
	})
	if err == nil {
		t.Fatal("expected error for malformed private key")
	}
}
