package terraform

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"

	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/testsupport"
	"github.com/IUCLabs/artifactapi/internal/tfsign"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

var testDSN string

func TestMain(m *testing.M) {
	ctx := context.Background()
	dsn, terminate, err := testsupport.StartPostgres(ctx)
	if err != nil {
		os.Exit(m.Run())
	}
	testDSN = dsn
	code := m.Run()
	terminate()
	os.Exit(code)
}

// testSigner writes a throwaway armored key and loads it.
func testSigner(t *testing.T) *tfsign.Signer {
	t.Helper()
	e, err := openpgp.NewEntity("artifactapi test", "tf", "tf@example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, _ := armor.Encode(&buf, openpgp.PrivateKeyType, nil)
	if err := e.SerializePrivate(w, nil); err != nil {
		t.Fatal(err)
	}
	w.Close()
	p := filepath.Join(t.TempDir(), "private-key.asc")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := tfsign.Load(p, "")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestProviderRegistryFlow(t *testing.T) {
	if testDSN == "" {
		t.Skip("Docker unavailable")
	}
	ctx := context.Background()
	db, err := database.New(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const repo = "tf-reg" // Terraform namespace == repo name
	const filePath = "myorg/artifactapi/terraform-provider-artifactapi_1.2.3_linux_amd64.zip"
	const hash = "sha256:983cdb25cb7b976538e4334d26e52dee5f44749b9be1500c760cf5cf66be659b"
	const wantSha = "983cdb25cb7b976538e4334d26e52dee5f44749b9be1500c760cf5cf66be659b"

	if err := db.CreateRemote(ctx, &models.Remote{Name: repo, PackageType: models.PackageTerraform, RepoType: models.RepoTypeLocal}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertBlob(ctx, hash, "blobs/98/3c", 6381007, "application/zip"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateLocalFile(ctx, repo, filePath, hash); err != nil {
		t.Fatal(err)
	}

	signer := testSigner(t)
	h := NewHandler(db, signer, "5.0,6.0")
	router := chi.NewRouter()
	router.Get("/.well-known/terraform.json", h.ServiceDiscovery)
	router.Mount(MountPath, h.Routes())

	get := func(p string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", p, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	// Service discovery.
	w := get("/.well-known/terraform.json")
	if w.Code != 200 {
		t.Fatalf("discovery = %d", w.Code)
	}
	var disc map[string]string
	json.Unmarshal(w.Body.Bytes(), &disc)
	if disc["providers.v1"] != ProvidersV1Path {
		t.Errorf("providers.v1 = %q", disc["providers.v1"])
	}

	// Versions.
	w = get("/terraform/v1/providers/tf-reg/artifactapi/versions")
	if w.Code != 200 {
		t.Fatalf("versions = %d %s", w.Code, w.Body)
	}
	var vresp struct {
		Versions []struct {
			Version   string              `json:"version"`
			Protocols []string            `json:"protocols"`
			Platforms []map[string]string `json:"platforms"`
		} `json:"versions"`
	}
	json.Unmarshal(w.Body.Bytes(), &vresp)
	if len(vresp.Versions) != 1 || vresp.Versions[0].Version != "1.2.3" {
		t.Fatalf("unexpected versions: %+v", vresp)
	}
	if len(vresp.Versions[0].Platforms) != 1 || vresp.Versions[0].Platforms[0]["os"] != "linux" {
		t.Fatalf("unexpected platforms: %+v", vresp.Versions[0].Platforms)
	}

	// Download.
	w = get("/terraform/v1/providers/tf-reg/artifactapi/1.2.3/download/linux/amd64")
	if w.Code != 200 {
		t.Fatalf("download = %d %s", w.Code, w.Body)
	}
	var dl struct {
		Filename            string `json:"filename"`
		DownloadURL         string `json:"download_url"`
		SHASumsURL          string `json:"shasums_url"`
		SHASumsSignatureURL string `json:"shasums_signature_url"`
		SHASum              string `json:"shasum"`
		SigningKeys         struct {
			GPGPublicKeys []struct {
				KeyID      string `json:"key_id"`
				ASCIIArmor string `json:"ascii_armor"`
			} `json:"gpg_public_keys"`
		} `json:"signing_keys"`
	}
	json.Unmarshal(w.Body.Bytes(), &dl)
	if dl.SHASum != wantSha {
		t.Errorf("shasum = %q", dl.SHASum)
	}
	wantURL := "http://example.com/api/v1/local/tf-reg/" + filePath
	if dl.DownloadURL != wantURL {
		t.Errorf("download_url = %q, want %q", dl.DownloadURL, wantURL)
	}
	if len(dl.SigningKeys.GPGPublicKeys) != 1 || dl.SigningKeys.GPGPublicKeys[0].KeyID != signer.KeyID() {
		t.Errorf("signing key mismatch: %+v", dl.SigningKeys)
	}

	// SHA256SUMS + signature verify against the advertised key.
	sums := get("/terraform/v1/providers/tf-reg/artifactapi/1.2.3/sha256sums")
	wantLine := wantSha + "  terraform-provider-artifactapi_1.2.3_linux_amd64.zip\n"
	if sums.Body.String() != wantLine {
		t.Errorf("sha256sums = %q, want %q", sums.Body.String(), wantLine)
	}
	sig := get("/terraform/v1/providers/tf-reg/artifactapi/1.2.3/sha256sums.sig")
	keyring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader([]byte(dl.SigningKeys.GPGPublicKeys[0].ASCIIArmor)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openpgp.CheckDetachedSignature(keyring, bytes.NewReader(sums.Body.Bytes()), bytes.NewReader(sig.Body.Bytes())); err != nil {
		t.Errorf("sha256sums.sig did not verify: %v", err)
	}
}

func TestRegistryDisabledWithoutSigner(t *testing.T) {
	h := NewHandler(nil, nil, "")
	router := chi.NewRouter()
	router.Get("/.well-known/terraform.json", h.ServiceDiscovery)
	req := httptest.NewRequest("GET", "/.well-known/terraform.json", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Errorf("disabled discovery = %d, want 404", w.Code)
	}
}
