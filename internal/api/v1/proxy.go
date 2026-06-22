package v1

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	v2 "github.com/IUCLabs/artifactapi/internal/api/v2"
	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/proxy"
	"github.com/IUCLabs/artifactapi/internal/storage"
	"github.com/IUCLabs/artifactapi/internal/virtual"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

var semverRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[a-zA-Z0-9.]+)?$`)

type ProxyHandler struct {
	engine        *proxy.Engine
	virtualEngine *virtual.Engine
	db            *database.DB
	store         *storage.S3
	local         *v2.LocalHandler
}

func NewProxyHandler(engine *proxy.Engine, virtualEngine *virtual.Engine, db *database.DB, store *storage.S3, local *v2.LocalHandler) *ProxyHandler {
	return &ProxyHandler{engine: engine, virtualEngine: virtualEngine, db: db, store: store, local: local}
}

func (h *ProxyHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/remote/{remoteName}/*", h.handleProxy)
	r.Get("/local/{localName}/*", h.handleLocal)
	r.Get("/virtual/{virtualName}/*", h.handleVirtual)
	return r
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

	result, err := h.engine.Fetch(r.Context(), *remote, path, prov)
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

	w.Header().Set("Content-Type", result.ContentType)
	w.Header().Set("X-Artifact-Source", result.Source)
	if result.Size > 0 {
		w.Header().Set("X-Artifact-Size", fmt.Sprintf("%d", result.Size))
	}
	w.WriteHeader(http.StatusOK)
	io.Copy(w, result.Reader)
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

	body, contentType, err := h.virtualEngine.Fetch(r.Context(), *virt, path, proxyBaseURL)
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

	if remote.PackageType == models.PackageTerraform {
		if h.serveTerraformMirror(w, r, remote, path) {
			return
		}
	}

	h.serveLocalFile(w, r, localName, path)
}

func (h *ProxyHandler) serveTerraformMirror(w http.ResponseWriter, r *http.Request, remote *models.Remote, path string) bool {
	parts := strings.Split(path, "/")
	if len(parts) < 3 {
		return false
	}

	namespace, typeName := parts[0], parts[1]
	tail := parts[2]

	if tail == "index.json" {
		h.local.ServeTerraformIndex(w, r, remote.Name, namespace, typeName)
		return true
	}

	if strings.HasSuffix(tail, ".json") {
		version := strings.TrimSuffix(tail, ".json")
		if semverRe.MatchString(version) {
			h.local.ServeTerraformVersionDoc(w, r, remote.Name, namespace, typeName, version)
			return true
		}
	}

	return false
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
