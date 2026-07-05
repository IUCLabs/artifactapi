package v1

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/storage"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// This file implements the write half of the Docker Registry HTTP API V2 for
// *local* docker repositories, so a `docker push` / `docker pull` against
// artifactapi treats a local docker repo as a genuine registry (matching the
// project's "local repos are the real thing" principle) rather than a mirror.
//
// Storage reuses the existing content-addressable primitives:
//   - blob and manifest bytes are stored via the CAS (deduplicated by sha256)
//   - a local_files row per (repo, "<image>/blobs/<digest>") and
//     (repo, "<image>/manifests/<ref>") keeps the blob referenced so the GC
//     does not reap it, and lets pulls resolve a reference back to a blob.
// Tags are mutable references (UpsertLocalFile); digests and blobs are
// immutable (CreateLocalFile, tolerating an already-exists on re-push).

const dockerAPIVersionHeader = "registry/2.0"

// uploadSession is an in-progress chunked blob upload, buffered to a temp file
// on disk. Sessions are held in-memory keyed by upload UUID, so a single push's
// PATCH/PUT chunks must be served by the same replica — true for the
// homelab single-instance deployment. Monolithic uploads avoid this entirely.
type uploadSession struct {
	file *os.File
	size int64
}

type uploadStore struct {
	mu       sync.Mutex
	sessions map[string]*uploadSession
}

func newUploadStore() *uploadStore {
	return &uploadStore{sessions: make(map[string]*uploadSession)}
}

func (s *uploadStore) create() (string, *uploadSession, error) {
	f, err := os.CreateTemp("", "docker-upload-*")
	if err != nil {
		return "", nil, err
	}
	id := uuid.NewString()
	sess := &uploadSession{file: f}
	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()
	return id, sess, nil
}

func (s *uploadStore) get(id string) *uploadSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

func (s *uploadStore) remove(id string) {
	s.mu.Lock()
	sess := s.sessions[id]
	delete(s.sessions, id)
	s.mu.Unlock()
	if sess != nil {
		sess.file.Close()
		os.Remove(sess.file.Name())
	}
}

// dockerReq is a parsed /v2/<remote>/<image>/... request. kind is one of
// "manifest", "blob", "upload", "tags".
type dockerReq struct {
	image string
	kind  string
	ref   string // tag, digest, or upload uuid depending on kind
}

// parseDockerPath splits the chi "*" remainder (everything after the repo name)
// into the image name and the registry operation. The image name may itself
// contain slashes, so operations are located by their well-known infixes.
func parseDockerPath(rest string) (dockerReq, bool) {
	rest = strings.TrimPrefix(rest, "/")
	switch {
	case strings.HasSuffix(rest, "/tags/list"):
		return dockerReq{image: strings.TrimSuffix(rest, "/tags/list"), kind: "tags"}, true
	case rest == "tags/list":
		return dockerReq{}, false // no image
	}
	if i := strings.Index(rest, "/blobs/uploads"); i >= 0 {
		image := rest[:i]
		ref := strings.TrimPrefix(rest[i+len("/blobs/uploads"):], "/")
		return dockerReq{image: image, kind: "upload", ref: ref}, image != ""
	}
	if i := strings.LastIndex(rest, "/manifests/"); i >= 0 {
		return dockerReq{image: rest[:i], kind: "manifest", ref: rest[i+len("/manifests/"):]}, true
	}
	if i := strings.LastIndex(rest, "/blobs/"); i >= 0 {
		return dockerReq{image: rest[:i], kind: "blob", ref: rest[i+len("/blobs/"):]}, true
	}
	return dockerReq{}, false
}

func isDigest(ref string) bool { return strings.HasPrefix(ref, "sha256:") }

// localDockerRemote returns the repo if name is a local docker repository.
func (h *ProxyHandler) localDockerRemote(r *http.Request, name string) (*models.Remote, bool) {
	remote, err := h.db.GetRemote(r.Context(), name)
	if err != nil {
		return nil, false
	}
	return remote, remote.RepoType == models.RepoTypeLocal && remote.PackageType == models.PackageDocker
}

func dockerError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Docker-Distribution-Api-Version", dockerAPIVersionHeader)
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"errors":[{"code":%q,"message":%q}]}`, code, msg)
}

// dockerGet dispatches a registry GET to the local handler for local docker
// repos and falls through to the upstream proxy for everything else.
func (h *ProxyHandler) dockerGet(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "remoteName")
	if remote, ok := h.localDockerRemote(r, name); ok {
		h.dockerLocalGet(w, r, remote, false)
		return
	}
	h.handleProxy(w, r)
}

func (h *ProxyHandler) dockerHead(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "remoteName")
	if remote, ok := h.localDockerRemote(r, name); ok {
		h.dockerLocalGet(w, r, remote, true)
		return
	}
	h.handleProxyHead(w, r)
}

func (h *ProxyHandler) dockerPost(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "remoteName")
	remote, ok := h.localDockerRemote(r, name)
	if !ok {
		dockerError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "push is only supported for local docker repositories")
		return
	}
	h.dockerStartUpload(w, r, remote)
}

func (h *ProxyHandler) dockerPatch(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "remoteName")
	remote, ok := h.localDockerRemote(r, name)
	if !ok {
		dockerError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "push is only supported for local docker repositories")
		return
	}
	h.dockerPatchUpload(w, r, remote)
}

func (h *ProxyHandler) dockerPut(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "remoteName")
	remote, ok := h.localDockerRemote(r, name)
	if !ok {
		dockerError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "push is only supported for local docker repositories")
		return
	}
	req, ok := parseDockerPath(chi.URLParam(r, "*"))
	if !ok {
		dockerError(w, http.StatusNotFound, "NAME_UNKNOWN", "unrecognised registry path")
		return
	}
	switch req.kind {
	case "upload":
		h.dockerFinishUpload(w, r, remote, req)
	case "manifest":
		h.dockerPutManifest(w, r, remote, req)
	default:
		dockerError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "PUT not supported for this path")
	}
}

func (h *ProxyHandler) dockerDelete(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "remoteName")
	remote, ok := h.localDockerRemote(r, name)
	if !ok {
		dockerError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "delete is only supported for local docker repositories")
		return
	}
	req, ok := parseDockerPath(chi.URLParam(r, "*"))
	if !ok || (req.kind != "manifest" && req.kind != "blob") {
		dockerError(w, http.StatusNotFound, "NAME_UNKNOWN", "unrecognised registry path")
		return
	}
	filePath := req.image + "/" + req.kind + "s/" + req.ref
	if err := h.db.DeleteLocalFile(r.Context(), remote.Name, filePath); err != nil {
		dockerError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
		return
	}
	w.Header().Set("Docker-Distribution-Api-Version", dockerAPIVersionHeader)
	w.WriteHeader(http.StatusAccepted)
}

// dockerLocalGet serves manifest / blob / tags-list reads for a local repo.
func (h *ProxyHandler) dockerLocalGet(w http.ResponseWriter, r *http.Request, remote *models.Remote, head bool) {
	req, ok := parseDockerPath(chi.URLParam(r, "*"))
	if !ok {
		dockerError(w, http.StatusNotFound, "NAME_UNKNOWN", "unrecognised registry path")
		return
	}
	switch req.kind {
	case "tags":
		h.dockerTagsList(w, r, remote, req.image)
	case "manifest":
		h.dockerServeRef(w, r, remote, req.image+"/manifests/"+req.ref, head, true)
	case "blob":
		h.dockerServeRef(w, r, remote, req.image+"/blobs/"+req.ref, head, false)
	default:
		dockerError(w, http.StatusNotFound, "NAME_UNKNOWN", "unrecognised registry path")
	}
}

// dockerServeRef streams the blob backing a local_files path. isManifest
// controls only the default content type; the stored blob content type wins.
func (h *ProxyHandler) dockerServeRef(w http.ResponseWriter, r *http.Request, remote *models.Remote, filePath string, head, isManifest bool) {
	file, err := h.db.GetLocalFile(r.Context(), remote.Name, filePath)
	if err != nil {
		dockerError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
		return
	}
	if file == nil {
		code := "BLOB_UNKNOWN"
		if isManifest {
			code = "MANIFEST_UNKNOWN"
		}
		dockerError(w, http.StatusNotFound, code, "not found")
		return
	}

	s3Key := storage.BlobKey(file.ContentHash[len("sha256:"):])
	reader, info, err := h.store.Download(r.Context(), s3Key)
	if err != nil {
		dockerError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
		return
	}
	defer reader.Close()

	contentType := info.ContentType
	if contentType == "" {
		if isManifest {
			contentType = "application/vnd.docker.distribution.manifest.v2+json"
		} else {
			contentType = "application/octet-stream"
		}
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size))
	w.Header().Set("Docker-Content-Digest", file.ContentHash)
	w.Header().Set("Docker-Distribution-Api-Version", dockerAPIVersionHeader)
	w.Header().Set("X-Artifact-Source", "local")
	if head {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	io.Copy(w, reader)
}

func (h *ProxyHandler) dockerTagsList(w http.ResponseWriter, r *http.Request, remote *models.Remote, image string) {
	prefix := image + "/manifests/"
	files, err := h.db.ListLocalFilesByPrefix(r.Context(), remote.Name, prefix)
	if err != nil {
		dockerError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
		return
	}
	tags := []string{}
	for _, f := range files {
		ref := strings.TrimPrefix(f.FilePath, prefix)
		if ref == "" || isDigest(ref) {
			continue
		}
		tags = append(tags, ref)
	}
	sort.Strings(tags)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Docker-Distribution-Api-Version", dockerAPIVersionHeader)
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"name":%q,"tags":`, remote.Name+"/"+image)
	writeJSONStringList(w, tags)
	fmt.Fprint(w, "}")
}

func writeJSONStringList(w io.Writer, items []string) {
	fmt.Fprint(w, "[")
	for i, s := range items {
		if i > 0 {
			fmt.Fprint(w, ",")
		}
		fmt.Fprintf(w, "%q", s)
	}
	fmt.Fprint(w, "]")
}

// dockerStartUpload begins a blob upload. It honours a monolithic
// POST?digest=... (blob in the POST body) and otherwise opens a chunked
// session, returning its Location for the client's PATCH/PUT.
func (h *ProxyHandler) dockerStartUpload(w http.ResponseWriter, r *http.Request, remote *models.Remote) {
	req, ok := parseDockerPath(chi.URLParam(r, "*"))
	if !ok || req.kind != "upload" {
		dockerError(w, http.StatusNotFound, "NAME_UNKNOWN", "unrecognised registry path")
		return
	}

	if digest := r.URL.Query().Get("digest"); digest != "" {
		h.dockerCommitBlob(w, r, remote, req.image, digest, r.Body)
		return
	}

	id, _, err := h.uploads.create()
	if err != nil {
		dockerError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
		return
	}
	loc := fmt.Sprintf("/v2/%s/%s/blobs/uploads/%s", remote.Name, req.image, id)
	w.Header().Set("Location", loc)
	w.Header().Set("Docker-Upload-UUID", id)
	w.Header().Set("Range", "0-0")
	w.Header().Set("Docker-Distribution-Api-Version", dockerAPIVersionHeader)
	w.WriteHeader(http.StatusAccepted)
}

func (h *ProxyHandler) dockerPatchUpload(w http.ResponseWriter, r *http.Request, remote *models.Remote) {
	req, ok := parseDockerPath(chi.URLParam(r, "*"))
	if !ok || req.kind != "upload" || req.ref == "" {
		dockerError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "unknown upload")
		return
	}
	sess := h.uploads.get(req.ref)
	if sess == nil {
		dockerError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "unknown upload")
		return
	}
	n, err := io.Copy(sess.file, r.Body)
	if err != nil {
		dockerError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
		return
	}
	sess.size += n
	loc := fmt.Sprintf("/v2/%s/%s/blobs/uploads/%s", remote.Name, req.image, req.ref)
	w.Header().Set("Location", loc)
	w.Header().Set("Docker-Upload-UUID", req.ref)
	w.Header().Set("Range", fmt.Sprintf("0-%d", sess.size-1))
	w.Header().Set("Docker-Distribution-Api-Version", dockerAPIVersionHeader)
	w.WriteHeader(http.StatusAccepted)
}

// dockerFinishUpload completes a chunked upload: appends any final PUT body,
// stores the assembled blob, and verifies its digest.
func (h *ProxyHandler) dockerFinishUpload(w http.ResponseWriter, r *http.Request, remote *models.Remote, req dockerReq) {
	digest := r.URL.Query().Get("digest")
	if digest == "" {
		dockerError(w, http.StatusBadRequest, "DIGEST_INVALID", "digest query parameter required")
		return
	}
	if req.ref == "" {
		// Monolithic PUT with no prior session: body is the whole blob.
		h.dockerCommitBlob(w, r, remote, req.image, digest, r.Body)
		return
	}
	sess := h.uploads.get(req.ref)
	if sess == nil {
		dockerError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "unknown upload")
		return
	}
	defer h.uploads.remove(req.ref)

	if _, err := io.Copy(sess.file, r.Body); err != nil {
		dockerError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
		return
	}
	if _, err := sess.file.Seek(0, io.SeekStart); err != nil {
		dockerError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
		return
	}
	h.dockerCommitBlob(w, r, remote, req.image, digest, sess.file)
}

// dockerCommitBlob stores blob bytes through the CAS, verifies the client's
// declared digest, and records the per-image local_files reference.
func (h *ProxyHandler) dockerCommitBlob(w http.ResponseWriter, r *http.Request, remote *models.Remote, image, digest string, body io.Reader) {
	result, err := h.cas.Store(r.Context(), body, "application/octet-stream")
	if err != nil {
		dockerError(w, http.StatusInternalServerError, "UNKNOWN", fmt.Sprintf("store failed: %v", err))
		return
	}
	if result.ContentHash != digest {
		dockerError(w, http.StatusBadRequest, "DIGEST_INVALID", fmt.Sprintf("digest mismatch: got %s, declared %s", result.ContentHash, digest))
		return
	}
	if err := h.db.UpsertBlob(r.Context(), result.ContentHash, result.S3Key, result.SizeBytes, "application/octet-stream"); err != nil {
		dockerError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
		return
	}
	if err := h.db.CreateLocalFile(r.Context(), remote.Name, image+"/blobs/"+digest, result.ContentHash); err != nil && !errors.Is(err, database.ErrAlreadyExists) {
		dockerError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
		return
	}
	w.Header().Set("Location", fmt.Sprintf("/v2/%s/%s/blobs/%s", remote.Name, image, digest))
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Docker-Distribution-Api-Version", dockerAPIVersionHeader)
	w.WriteHeader(http.StatusCreated)
}

// dockerPutManifest stores a manifest and points its reference (tag or digest)
// at it. Tags are mutable so a re-push moves the tag; digests are immutable.
func (h *ProxyHandler) dockerPutManifest(w http.ResponseWriter, r *http.Request, remote *models.Remote, req dockerReq) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		dockerError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
		return
	}
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/vnd.docker.distribution.manifest.v2+json"
	}
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	result, err := h.cas.Store(r.Context(), strings.NewReader(string(body)), contentType)
	if err != nil {
		dockerError(w, http.StatusInternalServerError, "UNKNOWN", fmt.Sprintf("store failed: %v", err))
		return
	}
	if err := h.db.UpsertBlob(r.Context(), result.ContentHash, result.S3Key, result.SizeBytes, contentType); err != nil {
		dockerError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
		return
	}
	// Always addressable by digest (immutable).
	if err := h.db.CreateLocalFile(r.Context(), remote.Name, req.image+"/manifests/"+digest, result.ContentHash); err != nil && !errors.Is(err, database.ErrAlreadyExists) {
		dockerError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
		return
	}
	// If pushed under a tag, (re)point the tag at this manifest.
	if !isDigest(req.ref) {
		if err := h.db.UpsertLocalFile(r.Context(), remote.Name, req.image+"/manifests/"+req.ref, result.ContentHash); err != nil {
			dockerError(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
			return
		}
	}

	slog.Info("local docker manifest pushed", "repo", remote.Name, "image", req.image, "ref", req.ref, "digest", digest)
	w.Header().Set("Location", fmt.Sprintf("/v2/%s/%s/manifests/%s", remote.Name, req.image, req.ref))
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Docker-Distribution-Api-Version", dockerAPIVersionHeader)
	w.WriteHeader(http.StatusCreated)
}
