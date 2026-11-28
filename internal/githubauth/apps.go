package githubauth

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

// AppStore loads API-managed GitHub Apps, private key included.
// *database.DB satisfies it.
type AppStore interface {
	GetGitHubApp(ctx context.Context, name string) (*models.GitHubApp, error)
}

type cachedApp struct {
	updatedAt time.Time
	cred      *appCredential
}

// Apps resolves API-managed GitHub Apps to credentials. Every lookup rereads
// the row so updates and deletes on any replica apply immediately; the minted
// credential (and its installation token) is reused while updated_at is unchanged.
type Apps struct {
	store  AppStore
	client httpDoer

	mu    sync.Mutex
	cache map[string]cachedApp
}

// NewApps builds an app registry over store.
func NewApps(store AppStore) *Apps {
	return &Apps{store: store, cache: map[string]cachedApp{}}
}

// Credential returns the credential for the named app.
func (a *Apps) Credential(ctx context.Context, name string) (Credential, error) {
	app, err := a.store.GetGitHubApp(ctx, name)
	if err != nil {
		a.mu.Lock()
		delete(a.cache, name)
		a.mu.Unlock()
		return nil, fmt.Errorf("github app %q: %w", name, err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.cache[name]; ok && c.updatedAt.Equal(app.UpdatedAt) {
		return c.cred, nil
	}
	key, err := ParsePrivateKey(app.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("github app %q: %w", name, err)
	}
	cred := newAppCredentialFromKey(strconv.FormatInt(app.AppID, 10), strconv.FormatInt(app.InstallationID, 10),
		key, app.APIBaseURL, a.client)
	a.cache[name] = cachedApp{updatedAt: app.UpdatedAt, cred: cred}
	return cred, nil
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
