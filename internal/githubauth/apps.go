package githubauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

// AppStore loads API-managed GitHub Apps. *database.DB satisfies it.
type AppStore interface {
	// GetGitHubApp returns the app, private key included.
	GetGitHubApp(ctx context.Context, name string) (*models.GitHubApp, error)
	// GitHubAppUpdatedAt returns only the app's updated_at.
	GitHubAppUpdatedAt(ctx context.Context, name string) (time.Time, error)
}

// AppRevalidateTTL is how long a cached app is served before its updated_at is
// rechecked; it bounds how stale an update or delete on another replica can be.
const AppRevalidateTTL = 5 * time.Second

type cachedApp struct {
	updatedAt time.Time
	checkedAt time.Time
	cred      *appCredential
}

// appReloadTimeout bounds a shared revalidation, which outlives any one caller.
const appReloadTimeout = 10 * time.Second

// Apps resolves API-managed GitHub Apps to credentials. A cached credential (and
// its installation token) is reused for AppRevalidateTTL, then revalidated with
// a key-less updated_at read; the key is only reloaded when updated_at moves.
// Revalidations are single-flighted per app name.
type Apps struct {
	store        AppStore
	allowedHosts []string
	client       httpDoer
	now          func() time.Time
	group        singleflight.Group

	mu    sync.Mutex
	cache map[string]cachedApp
}

// NewApps builds an app registry over store. A stored app whose api_base_url
// fails models.ValidateGitHubAPIBaseURL against allowedHosts is refused.
func NewApps(store AppStore, allowedHosts []string) *Apps {
	return &Apps{store: store, allowedHosts: allowedHosts, now: time.Now, cache: map[string]cachedApp{}}
}

// SetHTTPClient overrides the client used for token exchanges. Call before use.
func (a *Apps) SetHTTPClient(c *http.Client) {
	a.client = c
}

// Credential returns the credential for the named app. A missing app drops its
// cache entry; a transient store error serves a cached credential for another
// AppRevalidateTTL.
func (a *Apps) Credential(ctx context.Context, name string) (Credential, error) {
	if cred, ok := a.fresh(name); ok {
		return cred, nil
	}
	v, err, _ := a.group.Do(name, func() (any, error) {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), appReloadTimeout)
		defer cancel()
		return a.refresh(rctx, name)
	})
	if err != nil {
		return nil, err
	}
	return v.(Credential), nil
}

func (a *Apps) fresh(name string) (Credential, bool) {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.cache[name]
	if ok && now.Sub(c.checkedAt) < AppRevalidateTTL {
		return c.cred, true
	}
	return nil, false
}

func (a *Apps) refresh(ctx context.Context, name string) (Credential, error) {
	if cred, ok := a.fresh(name); ok {
		return cred, nil
	}
	now := a.now()
	a.mu.Lock()
	c, cached := a.cache[name]
	a.mu.Unlock()
	serveStale := func(op string, err error) Credential {
		slog.Warn("github app "+op+" failed; serving cached credential", "name", name, "err", err)
		return a.remember(name, cachedApp{updatedAt: c.updatedAt, checkedAt: now, cred: c.cred})
	}

	if cached {
		updatedAt, err := a.store.GitHubAppUpdatedAt(ctx, name)
		switch {
		case errors.Is(err, models.ErrGitHubAppNotFound):
			a.forget(name)
			return nil, fmt.Errorf("github app %q: %w", name, err)
		case err != nil:
			return serveStale("revalidation", err), nil
		case updatedAt.Equal(c.updatedAt):
			return a.remember(name, cachedApp{updatedAt: updatedAt, checkedAt: now, cred: c.cred}), nil
		}
	}

	app, err := a.store.GetGitHubApp(ctx, name)
	if errors.Is(err, models.ErrGitHubAppNotFound) {
		a.forget(name)
		return nil, fmt.Errorf("github app %q: %w", name, err)
	}
	if err != nil {
		if cached {
			return serveStale("reload", err), nil
		}
		return nil, fmt.Errorf("github app %q: %w", name, err)
	}
	apiBase := app.APIBaseURL
	if apiBase == "" {
		apiBase = models.DefaultGitHubAPIBaseURL
	}
	if err := models.ValidateGitHubAPIBaseURL(apiBase, a.allowedHosts); err != nil {
		a.forget(name)
		return nil, fmt.Errorf("github app %q: %w", name, err)
	}
	key, err := ParsePrivateKey(app.PrivateKey)
	if err != nil {
		a.forget(name)
		return nil, fmt.Errorf("github app %q: %w", name, err)
	}
	cred := newAppCredentialFromKey(strconv.FormatInt(app.AppID, 10), strconv.FormatInt(app.InstallationID, 10),
		key, apiBase, a.client)
	return a.remember(name, cachedApp{updatedAt: app.UpdatedAt, checkedAt: now, cred: cred}), nil
}

// remember caches e unless the entry already holds a newer version, and returns
// the credential now cached. An equal version keeps the existing credential so
// its installation token survives concurrent reloads.
func (a *Apps) remember(name string, e cachedApp) Credential {
	a.mu.Lock()
	defer a.mu.Unlock()
	if cur, ok := a.cache[name]; ok && !e.updatedAt.After(cur.updatedAt) {
		if cur.updatedAt.After(e.updatedAt) {
			return cur.cred
		}
		e.cred = cur.cred
		if cur.checkedAt.After(e.checkedAt) {
			e.checkedAt = cur.checkedAt
		}
	}
	a.cache[name] = e
	return e.cred
}

func (a *Apps) forget(name string) {
	a.mu.Lock()
	delete(a.cache, name)
	a.mu.Unlock()
}

var (
	appsMu sync.RWMutex
	apps   *Apps
)

// SetApps installs the process-wide app registry. Call once during startup.
func SetApps(a *Apps) {
	appsMu.Lock()
	apps = a
	appsMu.Unlock()
}

// AppRegistry returns the process-wide app registry, or nil if none is installed.
func AppRegistry() *Apps {
	appsMu.RLock()
	defer appsMu.RUnlock()
	return apps
}

var errNoAppRegistry = errors.New("github app registry not configured")

// AppToken mints a token for the named app from the given registry.
func AppToken(ctx context.Context, reg *Apps, name string) (string, error) {
	if reg == nil {
		return "", fmt.Errorf("github app %q: %w", name, errNoAppRegistry)
	}
	c, err := reg.Credential(ctx, name)
	if err != nil {
		return "", err
	}
	return c.Token(ctx)
}

// RemoteToken resolves the bearer token for a remote's GitHub requests:
// github_app, then the remote's password, then its username, then server,
// else anonymous ("").
func RemoteToken(ctx context.Context, reg *Apps, remote models.Remote, server Credential) (string, error) {
	switch {
	case remote.GitHubApp != "":
		return AppToken(ctx, reg, remote.GitHubApp)
	case remote.Password != "":
		return remote.Password, nil
	case remote.Username != "":
		return remote.Username, nil
	case server != nil:
		return server.Token(ctx)
	}
	return "", nil
}
