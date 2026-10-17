package v1

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	v2 "github.com/IUCLabs/artifactapi/internal/api/v2"
	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/proxy"
	"github.com/IUCLabs/artifactapi/internal/storage"
	"github.com/IUCLabs/artifactapi/internal/virtual"
)

type ProxyHandler struct {
	engine        *proxy.Engine
	virtualEngine *virtual.Engine
	db            *database.DB
	store         *storage.S3
	local         *v2.LocalHandler
	cas           *storage.CAS
}

func NewProxyHandler(engine *proxy.Engine, virtualEngine *virtual.Engine, db *database.DB, store *storage.S3, local *v2.LocalHandler) *ProxyHandler {
	return &ProxyHandler{
		engine:        engine,
		virtualEngine: virtualEngine,
		db:            db,
		store:         store,
		local:         local,
		cas:           storage.NewCAS(store),
	}
}

func (h *ProxyHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/remote/{remoteName}/*", h.handleProxy)
	r.Get("/local/{localName}/*", h.handleLocal)
	r.Get("/virtual/{virtualName}/*", h.handleVirtual)
	return r
}

// DockerV2Routes mounts the Docker Registry HTTP API V2. Reads (GET/HEAD)
// dispatch to a local registry implementation for local docker repos and fall
// through to the upstream proxy otherwise; writes (POST/PATCH/PUT/DELETE) are
// only valid for local docker repos and drive push.
func (h *ProxyHandler) DockerV2Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.handleDockerPing)
	r.Head("/", h.handleDockerPing)
	r.Get("/{remoteName}/*", h.dockerGet)
	r.Head("/{remoteName}/*", h.dockerHead)
	r.Post("/{remoteName}/*", h.dockerPost)
	r.Patch("/{remoteName}/*", h.dockerPatch)
	r.Put("/{remoteName}/*", h.dockerPut)
	r.Delete("/{remoteName}/*", h.dockerDelete)
	return r
}

func (h *ProxyHandler) handleDockerPing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.WriteHeader(http.StatusOK)
}

func (h *ProxyHandler) handleProxy(w http.ResponseWriter, r *http.Request) {
	remoteName := chi.URLParam(r, "remoteName")
	path := chi.URLParam(r, "*")

	remote, err := h.db.GetRemote(r.Context(), remoteName)
	if err != nil {
		http.Error(w, fmt.Sprintf("remote %q not found", remoteName), http.StatusNotFound)
		return
	}

	prov, err := provider.Get(remote.PackageType)
	if err != nil {
		http.Error(w, fmt.Sprintf("no provider for %q", remote.PackageType), http.StatusInternalServerError)
		return
	}

	// Metadata-only remotes (e.g. github_rpm) synthesize their own responses and
	// redirect package downloads to a backend remote instead of proxying bytes.
	if rs, ok := prov.(provider.RemoteServer); ok {
		proxyBaseURL := fmt.Sprintf("%s://%s", scheme(r), r.Host)
		if rs.ServeRemote(w, r, *remote, path, proxyBaseURL, h.db) {
			return
		}
	}

	result, err := h.engine.Fetch(r.Context(), *remote, path, prov, r.Header)
	if err != nil {
		var proxyErr *proxy.ProxyError
		if errors.As(err, &proxyErr) {
			http.Error(w, proxyErr.Message, proxyErr.Status)
			return
		}
		slog.Error("proxy fetch failed", "remote", remoteName, "path", path, "error", err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer result.Reader.Close()

	var body io.Reader = result.Reader
	if sr, ok := prov.(provider.ServeRewriter); ok && prov.Classify(path) == provider.Mutable {
		raw, err := io.ReadAll(result.Reader)
		if err != nil {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		raw = sr.RewriteServed(raw, *remote, fmt.Sprintf("%s://%s", scheme(r), r.Host))
		body, result.Size = bytes.NewReader(raw), int64(len(raw))
	}

	w.Header().Set("Content-Type", result.ContentType)
	w.Header().Set("X-Artifact-Source", result.Source)
	if result.Size > 0 {
		w.Header().Set("X-Artifact-Size", fmt.Sprintf("%d", result.Size))
	}
	w.WriteHeader(http.StatusOK)
	io.Copy(w, body)
}

func (h *ProxyHandler) handleProxyHead(w http.ResponseWriter, r *http.Request) {
	remoteName := chi.URLParam(r, "remoteName")
	path := chi.URLParam(r, "*")

	remote, err := h.db.GetRemote(r.Context(), remoteName)
	if err != nil {
		http.Error(w, fmt.Sprintf("remote %q not found", remoteName), http.StatusNotFound)
		return
	}

	prov, err := provider.Get(remote.PackageType)
	if err != nil {
		http.Error(w, fmt.Sprintf("no provider for %q", remote.PackageType), http.StatusInternalServerError)
		return
	}

	result, err := h.engine.Head(r.Context(), *remote, path, prov)
	if err != nil {
		var proxyErr *proxy.ProxyError
		if errors.As(err, &proxyErr) {
			http.Error(w, proxyErr.Message, proxyErr.Status)
			return
		}
		slog.Error("proxy head failed", "remote", remoteName, "path", path, "error", err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", result.ContentType)
	w.Header().Set("X-Artifact-Source", result.Source)
	if result.Size > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", result.Size))
	}
	w.WriteHeader(http.StatusOK)
}

func (h *ProxyHandler) handleVirtual(w http.ResponseWriter, r *http.Request) {
	virtualName := chi.URLParam(r, "virtualName")
	path := chi.URLParam(r, "*")

	virt, err := h.db.GetVirtual(r.Context(), virtualName)
	if err != nil {
		http.Error(w, fmt.Sprintf("virtual %q not found", virtualName), http.StatusNotFound)
		return
	}

	proxyBaseURL := fmt.Sprintf("%s://%s", scheme(r), r.Host)

	loc, ok, err := h.virtualEngine.MemberRedirect(r.Context(), *virt, path, r.URL.RawQuery, proxyBaseURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if ok {
		http.Redirect(w, r, loc, http.StatusFound)
		return
	}

	body, contentType, err := h.virtualEngine.Fetch(r.Context(), *virt, path, proxyBaseURL)
	if errors.Is(err, virtual.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("virtual fetch failed", "virtual", virtualName, "path", path, "error", err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Artifact-Source", "virtual")
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

func (h *ProxyHandler) handleLocal(w http.ResponseWriter, r *http.Request) {
	localName := chi.URLParam(r, "localName")
	path := chi.URLParam(r, "*")

	remote, err := h.db.GetRemote(r.Context(), localName)
	if err != nil {
		http.Error(w, fmt.Sprintf("local %q not found", localName), http.StatusNotFound)
		return
	}

	prov, _ := provider.Get(remote.PackageType)
	if indexer, ok := prov.(provider.LocalIndexer); ok {
		if indexer.ServeLocalIndex(w, r, h.db, remote.Name, path) {
			return
		}
	}

	h.serveLocalFile(w, r, localName, path)
}

func (h *ProxyHandler) serveLocalFile(w http.ResponseWriter, r *http.Request, repoName, path string) {
	file, err := h.db.GetLocalFile(r.Context(), repoName, path)
	if err != nil {
		slog.Error("local file lookup failed", "repo", repoName, "path", path, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if file == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	s3Key := storage.BlobKey(file.ContentHash[len("sha256:"):])
	reader, info, err := h.store.Download(r.Context(), s3Key)
	if err != nil {
		slog.Error("local file download failed", "repo", repoName, "path", path, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", info.ContentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size))
	w.Header().Set("X-Artifact-Source", "local")
	w.WriteHeader(http.StatusOK)
	io.Copy(w, reader)
}

func scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if fwd := r.Header.Get("X-Forwarded-Proto"); fwd != "" {
		return fwd
	}
	return "http"
}
