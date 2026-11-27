package v1

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	v2 "github.com/IUCLabs/artifactapi/internal/api/v2"
	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/provider/ansible"
	"github.com/IUCLabs/artifactapi/internal/proxy"
	"github.com/IUCLabs/artifactapi/internal/storage"
	"github.com/IUCLabs/artifactapi/internal/virtual"
	"github.com/IUCLabs/artifactapi/pkg/models"
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
	r.Head("/remote/{remoteName}/*", h.handleProxyHead)
	r.Get("/local/{localName}/*", h.handleLocal)
	r.Head("/local/{localName}/*", h.handleLocal)
	r.Put("/local/{localName}/*", h.handleLocalPut)
	r.Post("/local/{localName}/*", h.handleLocalPost)
	r.Delete("/local/{localName}/*", h.handleLocalDelete)
	r.Get("/virtual/{virtualName}/*", h.handleVirtual)
	r.Head("/virtual/{virtualName}/*", h.handleVirtual)
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

// remoteFor resolves the remote and provider named in the request, writing the
// error response itself when either is missing.
func (h *ProxyHandler) remoteFor(w http.ResponseWriter, r *http.Request) (*models.Remote, provider.Provider, bool) {
	remoteName := chi.URLParam(r, "remoteName")
	remote, err := h.db.GetRemote(r.Context(), remoteName)
	if err != nil {
		http.Error(w, fmt.Sprintf("remote %q not found", remoteName), http.StatusNotFound)
		return nil, nil, false
	}
	prov, err := provider.Get(remote.PackageType)
	if err != nil {
		http.Error(w, fmt.Sprintf("no provider for %q", remote.PackageType), http.StatusInternalServerError)
		return nil, nil, false
	}
	return remote, prov, true
}

// serveRemote lets metadata-only remotes (e.g. github_rpm) synthesize their own
// responses and redirect package downloads instead of proxying bytes.
func (h *ProxyHandler) serveRemote(w http.ResponseWriter, r *http.Request, remote *models.Remote, prov provider.Provider, path string) bool {
	rs, ok := prov.(provider.RemoteServer)
	return ok && rs.ServeRemote(w, r, *remote, path, fmt.Sprintf("%s://%s", Scheme(r), r.Host), h.db)
}

func (h *ProxyHandler) handleProxy(w http.ResponseWriter, r *http.Request) {
	remote, prov, ok := h.remoteFor(w, r)
	if !ok {
		return
	}
	path := chi.URLParam(r, "*")
	if h.serveRemote(w, r, remote, prov, path) {
		return
	}
	h.serveFetch(w, r, remote, prov, path)
}

func (h *ProxyHandler) serveFetch(w http.ResponseWriter, r *http.Request, remote *models.Remote, prov provider.Provider, path string) {
	result, err := h.engine.Fetch(r.Context(), *remote, forwardQuery(prov, path, r.URL.Query()), prov, r.Header)
	if err != nil {
		var proxyErr *proxy.ProxyError
		if errors.As(err, &proxyErr) {
			writeProxyError(w, proxyErr)
			return
		}
		slog.Error("proxy fetch failed", "remote", remote.Name, "path", path, "error", err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer func() { _ = result.Reader.Close() }()

	var body io.Reader = result.Reader
	if sr, ok := h.serveRewriter(r, *remote, prov); ok && prov.Classify(path) == provider.Mutable {
		raw, ok := rewriteServed(w, r, sr, *remote, result.Reader)
		if !ok {
			return
		}
		body, result.Size = bytes.NewReader(raw), int64(len(raw))
	}

	w.Header().Set("Content-Type", result.ContentType)
	w.Header().Set("X-Artifact-Source", result.Source)
	if result.Size > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", result.Size))
		w.Header().Set("X-Artifact-Size", fmt.Sprintf("%d", result.Size))
	}
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, body); err != nil {
		slog.Debug("response copy aborted", "path", r.URL.Path, "error", err)
	}
}

// writeProxyError answers a quarantine refusal as JSON and anything else as text.
func writeProxyError(w http.ResponseWriter, pe *proxy.ProxyError) {
	if pe.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(pe.RetryAfter))
	}
	if pe.Published.IsZero() {
		http.Error(w, pe.Message, pe.Status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(pe.Status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":     pe.Message,
		"published": pe.Published.Format(time.RFC3339),
		"available": pe.Available.Format(time.RFC3339),
	})
}

func rewritesMutable(prov provider.Provider, path string) bool {
	_, sr := prov.(provider.ServeRewriter)
	_, rel := prov.(provider.ReleasesRewriter)
	_, rr := prov.(provider.ResponseRewriter)
	return (sr || rel || rr) && prov.Classify(path) == provider.Mutable
}

type releasesRewriter struct {
	rw       provider.ReleasesRewriter
	releases models.Remote
}

func (r releasesRewriter) RewriteServed(body []byte, remote models.Remote, proxyBaseURL string) []byte {
	return r.rw.RewriteReleases(body, remote, r.releases, proxyBaseURL)
}

// serveRewriter returns the provider's per-request rewriter, binding the
// releases remote's config for providers that rewrite links onto it.
func (h *ProxyHandler) serveRewriter(r *http.Request, remote models.Remote, prov provider.Provider) (provider.ServeRewriter, bool) {
	if sr, ok := prov.(provider.ServeRewriter); ok {
		return sr, true
	}
	rw, ok := prov.(provider.ReleasesRewriter)
	if !ok {
		return nil, false
	}
	var releases models.Remote
	if remote.ReleasesRemote != "" {
		if rel, err := h.db.GetRemote(r.Context(), remote.ReleasesRemote); err == nil {
			releases = *rel
		} else {
			slog.Warn("releases remote lookup failed", "remote", remote.Name, "releases_remote", remote.ReleasesRemote, "error", err)
		}
	}
	return releasesRewriter{rw: rw, releases: releases}, true
}

// handleProxyHead answers HEAD like GET without a body. Responses a provider
// synthesizes or rewrites per request go through the GET path so their headers
// match; everything else is answered from store metadata or an upstream HEAD.
func (h *ProxyHandler) handleProxyHead(w http.ResponseWriter, r *http.Request) {
	remote, prov, ok := h.remoteFor(w, r)
	if !ok {
		return
	}
	path := chi.URLParam(r, "*")
	if h.serveRemote(w, r, remote, prov, path) {
		return
	}
	if rewritesMutable(prov, path) {
		h.serveFetch(w, r, remote, prov, path)
		return
	}

	result, err := h.engine.Head(r.Context(), *remote, path, prov)
	if err != nil {
		var proxyErr *proxy.ProxyError
		if errors.As(err, &proxyErr) {
			writeProxyError(w, proxyErr)
			return
		}
		slog.Error("proxy head failed", "remote", remote.Name, "path", path, "error", err)
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

	proxyBaseURL, ok := requestBaseURL(w, r)
	if !ok {
		return
	}

	if virt.PackageType == models.PackageGems {
		h.serveVirtualGems(w, r, *virt, path, proxyBaseURL)
		return
	}
	if virt.PackageType == models.PackageAnsible {
		h.serveVirtualAnsible(w, r, *virt, path, proxyBaseURL)
		return
	}
	if virt.PackageType == models.PackagePuppet {
		h.serveVirtualPuppet(w, r, *virt, path, proxyBaseURL)
		return
	}
	if virt.PackageType == models.PackageGoProxy {
		h.serveVirtualGoProxy(w, r, *virt, path)
		return
	}

	loc, ok, err := h.virtualEngine.MemberRedirect(r.Context(), *virt, path, r.URL.RawQuery, proxyBaseURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if ok {
		http.Redirect(w, r, loc, http.StatusFound)
		return
	}

	var body []byte
	var contentType string
	if virt.PackageType == models.PackageNPM {
		abbreviated := strings.Contains(r.Header.Get("Accept"), virtual.NPMAbbreviatedType)
		body, contentType, err = h.virtualEngine.FetchNPM(r.Context(), *virt, path, proxyBaseURL, abbreviated)
	} else {
		body, contentType, err = h.virtualEngine.Fetch(r.Context(), *virt, path, proxyBaseURL)
	}
	if errors.Is(err, virtual.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("virtual fetch failed", "virtual", virtualName, "path", path, "error", err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}

	if virt.PackageType == models.PackageNPM {
		setNPMPerHostHeaders(w)
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Artifact-Source", "virtual")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (h *ProxyHandler) handleLocal(w http.ResponseWriter, r *http.Request) {
	localName := chi.URLParam(r, "localName")
	path := chi.URLParam(r, "*")

	remote, err := h.db.GetRemote(r.Context(), localName)
	if err != nil {
		http.Error(w, fmt.Sprintf("local %q not found", localName), http.StatusNotFound)
		return
	}

	if remote.PackageType == models.PackageNPM {
		if pkg := npmPackageName(path); pkg != "" {
			h.serveNPMPackument(w, r, remote, pkg)
			return
		}
	}
	if remote.PackageType == models.PackageGems {
		if isGemsIndexPath(path) {
			h.serveLocalGemsIndex(w, r, remote, path)
			return
		}
		if strings.HasPrefix(path, gemsIndexPrefix) || strings.HasPrefix(path, gemsYankedPrefix) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	}

	if remote.PackageType == models.PackageAnsible {
		if ap := ansible.ParseAPIPath(path); ap.Kind != ansible.APIUnknown {
			h.serveLocalAnsible(w, r, remote, ap)
			return
		}
		if !strings.HasPrefix(path, ansible.ArtifactPrefix) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
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
	defer func() { _ = reader.Close() }()

	w.Header().Set("Content-Type", info.ContentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size))
	w.Header().Set("X-Artifact-Source", "local")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, reader); err != nil {
		slog.Debug("response copy aborted", "path", r.URL.Path, "error", err)
	}
}

// Scheme returns the client-facing scheme: TLS, else the first
// X-Forwarded-Proto value when it is http or https, else http.
func Scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	fwd, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	if fwd = strings.ToLower(strings.TrimSpace(fwd)); fwd == "https" {
		return fwd
	}
	return "http"
}

// forwardQuery appends the provider's validated query params to path in a
// canonical (sorted, single-valued) form, since the result is the cache key.
func forwardQuery(prov provider.Provider, path string, query url.Values) string {
	f, ok := prov.(provider.QueryForwarder)
	if !ok {
		return path
	}
	kept := url.Values{}
	for name, valid := range f.ForwardedQuery(path) {
		if v, ok := valid(query.Get(name)); ok {
			kept.Set(name, v)
		}
	}
	if len(kept) == 0 {
		return path
	}
	return path + "?" + kept.Encode()
}

// rewriteServed rewrites a cached body for the requesting host and marks the
// response uncacheable by shared caches, since it varies per Host.
func rewriteServed(w http.ResponseWriter, r *http.Request, sr provider.ServeRewriter, remote models.Remote, src io.Reader) ([]byte, bool) {
	base, ok := requestBaseURL(w, r)
	if !ok {
		return nil, false
	}
	raw, err := io.ReadAll(src)
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return nil, false
	}
	raw = sr.RewriteServed(raw, remote, base)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "Host, X-Forwarded-Proto")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	return raw, true
}

// requestBaseURL returns the client-facing scheme://host, answering 400 on an
// invalid Host.
func requestBaseURL(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !validHost(r.Host) {
		http.Error(w, "invalid host header", http.StatusBadRequest)
		return "", false
	}
	return Scheme(r) + "://" + r.Host, true
}

// validHost accepts host[:port] where host is a DNS name or IP literal.
func validHost(hostport string) bool {
	host, port := hostport, ""
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		if p == "" {
			return false
		}
		host, port = h, p
	} else if strings.HasPrefix(hostport, "[") {
		return strings.HasSuffix(hostport, "]") && net.ParseIP(hostport[1:len(hostport)-1]) != nil
	}
	if port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	if net.ParseIP(host) != nil {
		return true
	}
	if strings.HasPrefix(hostport, "[") {
		return false
	}
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' ||
			strings.Trim(strings.ToLower(label), "abcdefghijklmnopqrstuvwxyz0123456789-_") != "" {
			return false
		}
	}
	return true
}
