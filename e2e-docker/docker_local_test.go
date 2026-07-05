//go:build dockere2e

package e2edocker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
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

	// /v2/ version check.
	resp, _ := doRequest(t, http.MethodGet, api("/v2/"), nil, "")
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

	// A now-present blob HEAD should succeed (client would skip re-upload).
	resp, _ = doRequest(t, http.MethodHead, api("/v2/docker-internal/"+image+"/blobs/"+layerDigest), nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-push blob HEAD: expected 200, got %d", resp.StatusCode)
	}
}
