package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

func (e *Engine) Fetch(ctx context.Context, remote models.Remote, path string, prov provider.Provider) (*FetchResult, error) {
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

	start := time.Now()
	result, err := e.fetchFromUpstream(ctx, remote, path, prov, class, ttl)
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

func (e *Engine) fetchFromUpstream(ctx context.Context, remote models.Remote, path string, prov provider.Provider, class Classification, ttl time.Duration) (*FetchResult, error) {
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

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, &UpstreamError{Err: err}
	}

	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		token, err := fetchBearerToken(ctx, resp.Header.Get("Www-Authenticate"), remote)
		if err == nil && token != "" {
			req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			req2.Header.Set("Authorization", "Bearer "+token)
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

	contentType := prov.ContentType(path)
	if ct := resp.Header.Get("Content-Type"); ct != "" && contentType == "application/octet-stream" {
		contentType = ct
	}

	if class == ClassMutable {
		s3Key := storage.IndexKey(remote.Name, path)
		if err := e.store.Upload(ctx, s3Key, bytesReader(body), int64(len(body)), contentType); err != nil {
			return nil, fmt.Errorf("upload index: %w", err)
		}

		etag := resp.Header.Get("ETag")
		_ = e.cache.SetTTL(ctx, remote.Name, path, ttl)
		if etag != "" {
			_ = e.cache.SetETag(ctx, remote.Name, path, etag, ttl)
		}
	} else {
		hash := sha256Hash(body)
		s3Key := storage.BlobKey(hash)

		exists, _ := e.store.Exists(ctx, s3Key)
		if !exists {
			if err := e.store.Upload(ctx, s3Key, bytesReader(body), int64(len(body)), contentType); err != nil {
				return nil, fmt.Errorf("upload blob: %w", err)
			}
		}

		contentHash := fmt.Sprintf("sha256:%s", hash)
		if err := e.db.UpsertBlob(ctx, contentHash, s3Key, int64(len(body)), contentType); err != nil {
			slog.Warn("upsert blob failed", "error", err)
		}
		if err := e.db.UpsertArtifact(ctx, remote.Name, path, contentHash, resp.Header.Get("ETag")); err != nil {
			slog.Warn("upsert artifact failed", "error", err)
		}

		_ = e.cache.SetTTL(ctx, remote.Name, path, ttl)
		if etag := resp.Header.Get("ETag"); etag != "" {
			_ = e.cache.SetETag(ctx, remote.Name, path, etag, ttl)
		}
	}

	return &FetchResult{
		Reader:      io.NopCloser(bytesReader(body)),
		ContentType: contentType,
		Size:        int64(len(body)),
		Source:      "remote",
	}, nil
}

func (e *Engine) serveFromStore(ctx context.Context, remote models.Remote, path string) (*FetchResult, error) {
	artifact, err := e.db.GetArtifact(ctx, remote.Name, path)
	if err == nil && artifact != nil {
		reader, info, err := e.store.Download(ctx, artifact.ContentHash[len("sha256:"):])
		if err == nil {
			_ = e.db.TouchArtifactAccess(ctx, remote.Name, path)
			return &FetchResult{
				Reader:      reader,
				ContentType: info.ContentType,
				Size:        info.Size,
			}, nil
		}
		s3Key := storage.BlobKey(artifact.ContentHash[len("sha256:"):])
		reader, info, err = e.store.Download(ctx, s3Key)
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

func sha256Hash(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
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

func fetchBearerToken(ctx context.Context, wwwAuth string, remote models.Remote) (string, error) {
	if !strings.HasPrefix(wwwAuth, "Bearer ") {
		return "", fmt.Errorf("not a Bearer challenge")
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
		return "", fmt.Errorf("no realm in Bearer challenge")
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
		return "", err
	}

	if remote.Username != "" && remote.Password != "" {
		req.SetBasicAuth(remote.Username, remote.Password)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned %d", resp.StatusCode)
	}

	var tokenResp struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", err
	}

	if tokenResp.Token != "" {
		return tokenResp.Token, nil
	}
	return tokenResp.AccessToken, nil
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
	if _, ok := err.(*UpstreamError); ok {
		return true
	}
	return false
}
