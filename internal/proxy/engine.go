package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/IUCLabs/artifactapi/internal/cache"
	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/storage"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

const fetchLockTTL = 30 * time.Second

type Engine struct {
	db    *database.DB
	cache *cache.Redis
	store *storage.S3
	cas   *storage.CAS
}

func NewEngine(db *database.DB, c *cache.Redis, s *storage.S3) *Engine {
	return &Engine{
		db:    db,
		cache: c,
		store: s,
		cas:   storage.NewCAS(s),
	}
}

type FetchResult struct {
	Reader      io.ReadCloser
	ContentType string
	Size        int64
	Source      string // "cache" or "remote"
}

func (e *Engine) Fetch(ctx context.Context, remote models.Remote, path string, prov provider.Provider, clientHeaders ...http.Header) (*FetchResult, error) {
	classifier := NewClassifier(prov)
	class := classifier.Classify(remote, path)

	if class == ClassDenied {
		return nil, &ProxyError{Status: http.StatusForbidden, Message: "access denied"}
	}

	ttl := e.ttlFor(remote, class)

	fresh, err := e.cache.CheckTTL(ctx, remote.Name, path)
	if err != nil {
		slog.Warn("redis check failed, treating as miss", "error", err)
	}

	if fresh {
		result, err := e.serveFromStore(ctx, remote, path)
		if err == nil {
			result.Source = "cache"
			go e.logAccess(remote.Name, path, true, result.Size, 0)
			return result, nil
		}
		slog.Warn("cache hit but S3 miss, re-fetching", "remote", remote.Name, "path", path)
	}

	locked, err := e.cache.AcquireLock(ctx, remote.Name, path, fetchLockTTL)
	if err != nil {
		slog.Warn("lock acquire failed", "error", err)
	}

	if !locked {
		time.Sleep(500 * time.Millisecond)
		result, err := e.serveFromStore(ctx, remote, path)
		if err == nil {
			result.Source = "cache"
			go e.logAccess(remote.Name, path, true, result.Size, 0)
			return result, nil
		}
	}

	if locked {
		defer e.cache.ReleaseLock(ctx, remote.Name, path)
	}

	if class == ClassMutable && remote.CheckMutable {
		etag, _ := e.cache.GetETag(ctx, remote.Name, path)
		if etag != "" {
			notModified, err := e.checkUpstream(ctx, remote, path, etag, prov)
			if err == nil && notModified {
				_ = e.cache.SetTTL(ctx, remote.Name, path, ttl)
				_ = e.cache.SetETag(ctx, remote.Name, path, etag, ttl)
				result, err := e.serveFromStore(ctx, remote, path)
				if err == nil {
					result.Source = "cache"
					go e.logAccess(remote.Name, path, true, result.Size, 0)
					return result, nil
				}
			}
		}
	}

	var fwdHeaders http.Header
	if len(clientHeaders) > 0 && clientHeaders[0] != nil {
		fwdHeaders = clientHeaders[0]
	}

	start := time.Now()
	result, err := e.fetchFromUpstream(ctx, remote, path, prov, class, ttl, fwdHeaders)
	upstreamMS := int(time.Since(start).Milliseconds())
	if err != nil {
		if remote.StaleOnError && isNetworkError(err) {
			_ = e.cache.SetTTL(ctx, remote.Name, path, ttl)
			stale, serr := e.serveFromStore(ctx, remote, path)
			if serr == nil {
				slog.Warn("serving stale on upstream error", "remote", remote.Name, "path", path, "error", err)
				stale.Source = "cache"
				go e.logAccess(remote.Name, path, true, stale.Size, 0)
				return stale, nil
			}
		}
		return nil, err
	}

	go e.logAccess(remote.Name, path, false, result.Size, upstreamMS)
	return result, nil
}

func (e *Engine) fetchFromUpstream(ctx context.Context, remote models.Remote, path string, prov provider.Provider, class Classification, ttl time.Duration, clientHeaders http.Header) (*FetchResult, error) {
	url := prov.UpstreamURL(remote, path)

	authHeaders, err := prov.AuthHeaders(ctx, remote)
	if err != nil {
		return nil, fmt.Errorf("auth headers: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	for k, vv := range authHeaders {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	if clientHeaders != nil {
		if accept := clientHeaders.Get("Accept"); accept != "" {
			req.Header.Set("Accept", accept)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, &UpstreamError{Err: err}
	}

	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		token, err := e.cachedBearerToken(ctx, resp.Header.Get("Www-Authenticate"), remote)
		if err == nil && token != "" {
			req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			req2.Header.Set("Authorization", "Bearer "+token)
			if clientHeaders != nil {
				if accept := clientHeaders.Get("Accept"); accept != "" {
					req2.Header.Set("Accept", accept)
				}
			}
			resp, err = http.DefaultClient.Do(req2)
			if err != nil {
				return nil, &UpstreamError{Err: err}
			}
		} else {
			return nil, &ProxyError{Status: http.StatusUnauthorized, Message: "upstream returned 401"}
		}
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, &ProxyError{Status: resp.StatusCode, Message: fmt.Sprintf("upstream returned %d", resp.StatusCode)}
	}

	contentType := prov.ContentType(path)
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		contentType = ct
	}

	// Mutable indexes are small and may be rewritten, so buffer them in memory.
	if class == ClassMutable {
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read upstream body: %w", err)
		}

		rewritten, err := prov.RewriteResponse(body, remote, "")
		if err != nil {
			return nil, fmt.Errorf("rewrite response: %w", err)
		}
		if rewritten != nil {
			body = rewritten
		}

		s3Key := storage.IndexKey(remote.Name, path)
		if err := e.store.Upload(ctx, s3Key, bytesReader(body), int64(len(body)), contentType); err != nil {
			return nil, fmt.Errorf("upload index: %w", err)
		}

		_ = e.cache.SetTTL(ctx, remote.Name, path, ttl)
		if etag := resp.Header.Get("ETag"); etag != "" {
			_ = e.cache.SetETag(ctx, remote.Name, path, etag, ttl)
		}

		return &FetchResult{
			Reader:      io.NopCloser(bytesReader(body)),
			ContentType: contentType,
			Size:        int64(len(body)),
			Source:      "remote",
		}, nil
	}

	// Immutable blobs are streamed through the content-addressable store
	// (tempfile -> sha256 -> S3) so arbitrarily large artifacts never sit
	// fully in memory. Immutable content is never rewritten in the proxy path.
	casResult, err := e.cas.Store(ctx, resp.Body, contentType)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("store blob: %w", err)
	}

	if err := e.db.UpsertBlob(ctx, casResult.ContentHash, casResult.S3Key, casResult.SizeBytes, contentType); err != nil {
		slog.Warn("upsert blob failed", "error", err)
	}
	if err := e.db.UpsertArtifact(ctx, remote.Name, path, casResult.ContentHash, resp.Header.Get("ETag")); err != nil {
		slog.Warn("upsert artifact failed", "error", err)
	}

	_ = e.cache.SetTTL(ctx, remote.Name, path, ttl)
	if etag := resp.Header.Get("ETag"); etag != "" {
		_ = e.cache.SetETag(ctx, remote.Name, path, etag, ttl)
	}

	reader, info, err := e.store.Download(ctx, casResult.S3Key)
	if err != nil {
		return nil, fmt.Errorf("serve stored blob: %w", err)
	}
	return &FetchResult{
		Reader:      reader,
		ContentType: info.ContentType,
		Size:        casResult.SizeBytes,
		Source:      "remote",
	}, nil
}

func (e *Engine) serveFromStore(ctx context.Context, remote models.Remote, path string) (*FetchResult, error) {
	artifact, err := e.db.GetArtifact(ctx, remote.Name, path)
	if err == nil && artifact != nil {
		s3Key := storage.BlobKey(artifact.ContentHash[len("sha256:"):])
		reader, info, err := e.store.Download(ctx, s3Key)
		if err == nil {
			_ = e.db.TouchArtifactAccess(ctx, remote.Name, path)
			return &FetchResult{
				Reader:      reader,
				ContentType: info.ContentType,
				Size:        info.Size,
			}, nil
		}
	}

	s3Key := storage.IndexKey(remote.Name, path)
	reader, info, err := e.store.Download(ctx, s3Key)
	if err != nil {
		return nil, fmt.Errorf("not in store: %w", err)
	}
	return &FetchResult{
		Reader:      reader,
		ContentType: info.ContentType,
		Size:        info.Size,
	}, nil
}

func (e *Engine) checkUpstream(ctx context.Context, remote models.Remote, path, etag string, prov provider.Provider) (bool, error) {
	url := prov.UpstreamURL(remote, path)

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("If-None-Match", etag)

	authHeaders, err := prov.AuthHeaders(ctx, remote)
	if err != nil {
		return false, err
	}
	for k, vv := range authHeaders {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, &UpstreamError{Err: err}
	}
	resp.Body.Close()

	return resp.StatusCode == http.StatusNotModified, nil
}

func (e *Engine) ttlFor(remote models.Remote, class Classification) time.Duration {
	switch class {
	case ClassImmutable:
		if remote.ImmutableTTL == 0 {
			return 0
		}
		return time.Duration(remote.ImmutableTTL) * time.Second
	default:
		return time.Duration(remote.MutableTTL) * time.Second
	}
}

func (e *Engine) logAccess(remoteName, path string, cacheHit bool, size int64, upstreamMS int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = e.db.InsertAccessLog(ctx, remoteName, path, cacheHit, size, upstreamMS, "")
}

func bytesReader(data []byte) io.Reader {
	return io.NewSectionReader(readerAt(data), 0, int64(len(data)))
}

type readerAt []byte

func (r readerAt) ReadAt(p []byte, off int64) (n int, err error) {
	if off >= int64(len(r)) {
		return 0, io.EOF
	}
	n = copy(p, r[off:])
	if off+int64(n) >= int64(len(r)) {
		err = io.EOF
	}
	return
}

// bearerTokenTTLDefault/Margin bound how long a token is cached: the default
// is used when the token endpoint omits expires_in, and the margin is
// subtracted so a cached token is refreshed slightly before it actually expires.
const (
	bearerTokenTTLDefault = 60 * time.Second
	bearerTokenTTLMargin  = 10 * time.Second
)

// cachedBearerToken returns a bearer token for the given challenge, reusing a
// Redis-cached token for the same remote+challenge while it is still valid.
func (e *Engine) cachedBearerToken(ctx context.Context, wwwAuth string, remote models.Remote) (string, error) {
	key := remote.Name + ":" + sha256Hash([]byte(wwwAuth))
	if tok, err := e.cache.GetToken(ctx, key); err == nil && tok != "" {
		return tok, nil
	}

	tok, ttl, err := fetchBearerToken(ctx, wwwAuth, remote)
	if err != nil {
		return "", err
	}
	if tok != "" {
		if ttl <= 0 {
			ttl = bearerTokenTTLDefault
		}
		if ttl > bearerTokenTTLMargin {
			ttl -= bearerTokenTTLMargin
		}
		_ = e.cache.SetToken(ctx, key, tok, ttl)
	}
	return tok, nil
}

func fetchBearerToken(ctx context.Context, wwwAuth string, remote models.Remote) (string, time.Duration, error) {
	if !strings.HasPrefix(wwwAuth, "Bearer ") {
		return "", 0, fmt.Errorf("not a Bearer challenge")
	}

	params := map[string]string{}
	for _, part := range strings.Split(wwwAuth[7:], ",") {
		part = strings.TrimSpace(part)
		eq := strings.Index(part, "=")
		if eq < 0 {
			continue
		}
		key := part[:eq]
		val := strings.Trim(part[eq+1:], `"`)
		params[key] = val
	}

	realm := params["realm"]
	if realm == "" {
		return "", 0, fmt.Errorf("no realm in Bearer challenge")
	}

	tokenURL := realm
	sep := "?"
	if s, ok := params["service"]; ok {
		tokenURL += sep + "service=" + s
		sep = "&"
	}
	if s, ok := params["scope"]; ok {
		tokenURL += sep + "scope=" + s
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
	if err != nil {
		return "", 0, err
	}

	if remote.Username != "" && remote.Password != "" {
		req.SetBasicAuth(remote.Username, remote.Password)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("token endpoint returned %d", resp.StatusCode)
	}

	var tokenResp struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", 0, err
	}

	ttl := time.Duration(tokenResp.ExpiresIn) * time.Second
	if tokenResp.Token != "" {
		return tokenResp.Token, ttl, nil
	}
	return tokenResp.AccessToken, ttl, nil
}

type ProxyError struct {
	Status  int
	Message string
}

func (e *ProxyError) Error() string { return e.Message }

type UpstreamError struct {
	Err error
}

func (e *UpstreamError) Error() string { return fmt.Sprintf("upstream error: %v", e.Err) }
func (e *UpstreamError) Unwrap() error { return e.Err }

func isNetworkError(err error) bool {
	var ue *UpstreamError
	return errors.As(err, &ue)
}
