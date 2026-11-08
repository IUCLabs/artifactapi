//go:build dockere2e

package e2edocker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// pushBlobMonolithic uploads a blob with POST (open session) then PUT?digest
// (whole body) — the monolithic-after-POST flow.
func pushBlobMonolithic(t *testing.T, repo, image string, blob []byte) {
	t.Helper()
	dgst := digestOf(blob)

	resp, body := doRequest(t, http.MethodPost, api("/v2/"+repo+"/"+image+"/blobs/uploads/"), nil, "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start upload: status %d: %s", resp.StatusCode, body)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatalf("start upload: no Location header")
	}

	resp, body = doRequest(t, http.MethodPut, baseURL()+loc+"?digest="+dgst, blob, "application/octet-stream")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("finish upload: status %d: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Docker-Content-Digest"); got != dgst {
		t.Fatalf("finish upload: digest mismatch: got %q want %q", got, dgst)
	}
}

// pushBlobChunked uploads a blob with POST then PATCH (body) then PUT?digest
// (empty) — the chunked flow a real docker daemon uses.
func pushBlobChunked(t *testing.T, repo, image string, blob []byte) {
	t.Helper()
	dgst := digestOf(blob)

	resp, body := doRequest(t, http.MethodPost, api("/v2/"+repo+"/"+image+"/blobs/uploads/"), nil, "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start upload: status %d: %s", resp.StatusCode, body)
	}
	loc := resp.Header.Get("Location")

	resp, body = doRequest(t, http.MethodPatch, baseURL()+loc, blob, "application/octet-stream")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("patch upload: status %d: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Range"); got != fmt.Sprintf("0-%d", len(blob)-1) {
		t.Fatalf("patch upload: unexpected Range %q", got)
	}
	loc = resp.Header.Get("Location")

	resp, body = doRequest(t, http.MethodPut, baseURL()+loc+"?digest="+dgst, nil, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("finish upload: status %d: %s", resp.StatusCode, body)
	}
}

// TestLocalDockerPushPull exercises a full container push and pull against a
// local docker repo using the Docker Registry HTTP API V2, the way a docker
// client would: upload the config and layer blobs, push the manifest under a
// tag, then pull the manifest and blobs back byte-identically.
func TestLocalDockerPushPull(t *testing.T) {
	createRepo(t, `{"name":"docker-internal","package_type":"docker","repo_type":"local"}`)
	defer deleteRepo(t, "docker-internal")

	const image = "team/app"
	const tag = "v1.0.0"

	// /v2/ version check: anonymous gets the token challenge, a token passes.
	resp, _ := doRequestAuth(t, http.MethodGet, api("/v2/"), nil, "", "")
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "/v2/token") {
		t.Fatalf("anonymous /v2/ ping: status %d, challenge %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	resp, _ = doRequest(t, http.MethodGet, api("/v2/"), nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v2/ ping: status %d", resp.StatusCode)
	}

	config := []byte(`{"architecture":"amd64","os":"linux","config":{},"rootfs":{"type":"layers","diff_ids":["sha256:0000000000000000000000000000000000000000000000000000000000000000"]}}`)
	layer := bytes.Repeat([]byte("artifactapi-layer-data-"), 4096) // ~90 KB opaque layer

	configDigest := digestOf(config)
	layerDigest := digestOf(layer)

	// A brand-new blob should be absent (this is the client's mount check).
	resp, _ = doRequest(t, http.MethodHead, api("/v2/"+"docker-internal/"+image+"/blobs/"+configDigest), nil, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("pre-push blob HEAD: expected 404, got %d", resp.StatusCode)
	}

	pushBlobMonolithic(t, "docker-internal", image, config)
	pushBlobChunked(t, "docker-internal", image, layer)

	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{"mediaType":"application/vnd.docker.container.image.v1+json","size":%d,"digest":%q},"layers":[{"mediaType":"application/vnd.docker.image.rootfs.diff.tar.gzip","size":%d,"digest":%q}]}`,
		len(config), configDigest, len(layer), layerDigest))
	manifestDigest := digestOf(manifest)
	manifestType := "application/vnd.docker.distribution.manifest.v2+json"

	resp, body := doRequest(t, http.MethodPut, api("/v2/docker-internal/"+image+"/manifests/"+tag), manifest, manifestType)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("push manifest: status %d: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Docker-Content-Digest"); got != manifestDigest {
		t.Fatalf("push manifest: digest %q want %q", got, manifestDigest)
	}

	// --- pull back ---

	// Manifest by tag.
	resp, body = doRequest(t, http.MethodGet, api("/v2/docker-internal/"+image+"/manifests/"+tag), nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pull manifest by tag: status %d: %s", resp.StatusCode, body)
	}
	if !bytes.Equal(body, manifest) {
		t.Fatalf("pulled manifest bytes differ from pushed")
	}
	if ct := resp.Header.Get("Content-Type"); ct != manifestType {
		t.Fatalf("pulled manifest content-type %q want %q", ct, manifestType)
	}
	if got := resp.Header.Get("Docker-Content-Digest"); got != manifestDigest {
		t.Fatalf("pulled manifest digest %q want %q", got, manifestDigest)
	}

	// Manifest by digest.
	resp, body = doRequest(t, http.MethodGet, api("/v2/docker-internal/"+image+"/manifests/"+manifestDigest), nil, "")
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, manifest) {
		t.Fatalf("pull manifest by digest: status %d, equal=%v", resp.StatusCode, bytes.Equal(body, manifest))
	}

	// Blobs by digest.
	for _, tc := range []struct {
		name   string
		digest string
		want   []byte
	}{
		{"config", configDigest, config},
		{"layer", layerDigest, layer},
	} {
		resp, body = doRequest(t, http.MethodGet, api("/v2/docker-internal/"+image+"/blobs/"+tc.digest), nil, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("pull %s blob: status %d", tc.name, resp.StatusCode)
		}
		if !bytes.Equal(body, tc.want) {
			t.Fatalf("pulled %s blob bytes differ", tc.name)
		}
		if got := resp.Header.Get("Docker-Content-Digest"); got != tc.digest {
			t.Fatalf("pulled %s blob digest %q want %q", tc.name, got, tc.digest)
		}
	}

	// tags/list reflects the pushed tag.
	resp, body = doRequest(t, http.MethodGet, api("/v2/docker-internal/"+image+"/tags/list"), nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tags/list: status %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"`+tag+`"`) {
		t.Fatalf("tags/list missing tag %q: %s", tag, body)
	}
	if !strings.Contains(string(body), `"docker-internal/`+image+`"`) {
		t.Fatalf("tags/list wrong repository name: %s", body)
	}

	assertWriteGated(t, http.MethodPut, api("/v2/docker-internal/"+image+"/manifests/v2"), manifest, manifestType, api("/v2/docker-internal/"+image+"/manifests/"+tag))

	// A now-present blob HEAD should succeed (client would skip re-upload).
	resp, _ = doRequest(t, http.MethodHead, api("/v2/docker-internal/"+image+"/blobs/"+layerDigest), nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-push blob HEAD: expected 200, got %d", resp.StatusCode)
	}
}

// TestLocalDockerCLIPushPull drives the host docker CLI: docker login with the
// token as password then push, a push without login rejected, an anonymous
// pull, and a buildx (buildkit) push.
func TestLocalDockerCLIPushPull(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available on the test host")
	}
	createRepo(t, `{"name":"docker-cli","package_type":"docker","repo_type":"local"}`)
	defer deleteRepo(t, "docker-cli")

	reg := strings.TrimPrefix(baseURL(), "http://")
	img := reg + "/docker-cli/e2e/hello"
	cfg := t.TempDir()
	docker := func(stdin []byte, args ...string) (string, error) {
		cmd := exec.Command("docker", args...)
		cmd.Env = append(os.Environ(), "DOCKER_CONFIG="+cfg)
		cmd.Stdin = bytes.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	must := func(t *testing.T, stdin []byte, args ...string) {
		t.Helper()
		if out, err := docker(stdin, args...); err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	defer func() { _, _ = docker(nil, "rmi", "-f", img+":v1", img+":anon", img+":buildx") }()

	must(t, make([]byte, 1024), "import", "-", img+":v1") // an empty tar

	must(t, []byte(authToken), "login", reg, "-u", "ci", "--password-stdin")
	must(t, nil, "push", img+":v1")

	must(t, nil, "logout", reg)
	must(t, nil, "tag", img+":v1", img+":anon")
	if out, err := docker(nil, "push", img+":anon"); err == nil {
		t.Fatalf("push without login accepted:\n%s", out)
	}
	must(t, nil, "rmi", img+":v1", img+":anon")
	must(t, nil, "pull", img+":v1")

	t.Run("buildx", func(t *testing.T) {
		t.Skip("buildkit fetches the registry token with an OAuth2 POST form the server ignores; fixed by myorg/artifactapi#180")
		conf := cfg + "/buildkitd.toml"
		if err := os.WriteFile(conf, []byte(fmt.Sprintf("[registry.%q]\n  http = true\n", reg)), 0o644); err != nil {
			t.Fatal(err)
		}
		builder := fmt.Sprintf("e2e-%d", os.Getpid())
		must(t, nil, "buildx", "create", "--name", builder, "--driver", "docker-container", "--driver-opt", "network=host", "--buildkitd-config", conf)
		defer func() { _, _ = docker(nil, "buildx", "rm", "-f", builder) }()
		must(t, []byte(authToken), "login", reg, "-u", "ci", "--password-stdin")
		must(t, []byte("FROM "+img+":v1\nLABEL e2e=buildx\n"), "buildx", "build", "--builder", builder, "--push", "-t", img+":buildx", "-")
	})
}
