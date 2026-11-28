package v2

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/IUCLabs/artifactapi/internal/api/requrl"
	"github.com/IUCLabs/artifactapi/internal/auth"
	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/githubauth"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// ManifestStateTTL bounds each step of the manifest flow; GitHub's manifest
// code also expires after an hour.
const ManifestStateTTL = time.Hour

// UIPath is where the web UI is served; GitHub redirects land on its routes.
const UIPath = "/ui"

// StateStore holds single-use flow state shared across replicas. *cache.Redis
// satisfies it.
type StateStore interface {
	PutState(ctx context.Context, key string, val []byte, ttl time.Duration) error
	// GetState reads key without consuming it; missing or expired is nil.
	GetState(ctx context.Context, key string) ([]byte, error)
	// TakeStateIf atomically deletes key if it still holds val.
	TakeStateIf(ctx context.Context, key string, val []byte) (bool, error)
}

// ManifestAppStore persists apps created from a manifest. *database.DB satisfies it.
type ManifestAppStore interface {
	GetGitHubApp(ctx context.Context, name string) (*models.GitHubApp, error)
	CreateGitHubApp(ctx context.Context, a *models.GitHubApp) error
	SetGitHubAppInstallation(ctx context.Context, name string, appID, installationID int64) (*models.GitHubApp, error)
}

// GitHubAppManifestHandler serves the GitHub App Manifest create flow under
// /api/v2/github-apps/manifest. Private keys never leave the server.
type GitHubAppManifestHandler struct {
	store    ManifestAppStore
	states   StateStore
	apiHosts []string
	client   *http.Client
}

func NewGitHubAppManifestHandler(store ManifestAppStore, states StateStore, apiHosts []string, client *http.Client) *GitHubAppManifestHandler {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &GitHubAppManifestHandler{store: store, states: states, apiHosts: apiHosts, client: client}
}

func (h *GitHubAppManifestHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.start)
	r.Post("/convert", h.convert)
	r.Post("/installed", h.installed)
	return r
}

type manifestState struct {
	Name    string    `json:"name"`
	Owner   string    `json:"owner,omitempty"`
	WebBase string    `json:"web_base"`
	APIBase string    `json:"api_base"`
	Subject string    `json:"subject"`
	AppID   int64     `json:"app_id,omitempty"`
	Expires time.Time `json:"expires"`
}

const (
	createStatePrefix  = "github-app-manifest:create:"
	installStatePrefix = "github-app-manifest:install:"
)

var (
	githubLoginRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	manifestCode  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)
	stateRe       = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
)

// maxGitHubAppNameLen is GitHub's limit on an app's display name.
const maxGitHubAppNameLen = 34

// webBaseFor returns the web base URL of a stored app's API base URL.
func webBaseFor(apiBase string) string {
	u, err := url.Parse(apiBase)
	if err != nil || u.Host == "api.github.com" {
		return "https://github.com"
	}
	return "https://" + u.Host
}

// githubEndpoints maps a web host to its web and API base URLs: github.com, or
// a GHES host listed in GITHUB_APP_API_HOSTS served at /api/v3.
func githubEndpoints(host string, apiHosts []string) (webBase, apiBase string, err error) {
	if host == "" || host == "github.com" {
		return "https://github.com", models.DefaultGitHubAPIBaseURL, nil
	}
	h, err := models.ParseGitHubAPIHost(host)
	if err != nil {
		return "", "", err
	}
	if h == "github.com" {
		return "https://github.com", models.DefaultGitHubAPIBaseURL, nil
	}
	if !slices.Contains(apiHosts, h) {
		return "", "", fmt.Errorf("github_host %q is not allowed: it expects the GitHub web host (e.g. github.com, or a GHES host listed in GITHUB_APP_API_HOSTS), not the API host", h)
	}
	apiBase = "https://" + h + "/api/v3"
	if err := models.ValidateGitHubAPIBaseURL(apiBase, apiHosts); err != nil {
		return "", "", err
	}
	return "https://" + h, apiBase, nil
}

func newState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (h *GitHubAppManifestHandler) putState(ctx context.Context, prefix string, st manifestState) (string, error) {
	state, err := newState()
	if err != nil {
		return "", err
	}
	st.Expires = time.Now().Add(ManifestStateTTL)
	b, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	return state, h.states.PutState(ctx, prefix+state, b, ManifestStateTTL)
}

// issueInstallState stores an install state, retrying once since by now the
// GitHub App already exists.
func (h *GitHubAppManifestHandler) issueInstallState(ctx context.Context, st manifestState) (string, error) {
	state, err := h.putState(ctx, installStatePrefix, st)
	if err != nil {
		slog.Warn("github app manifest: store install state, retrying", "name", st.Name, "err", err)
		state, err = h.putState(ctx, installStatePrefix, st)
	}
	return state, err
}

// restoreState puts a consumed state back for its remaining lifetime so the
// caller can retry after a transient failure.
func (h *GitHubAppManifestHandler) restoreState(ctx context.Context, prefix, state string, st manifestState) {
	ttl := time.Until(st.Expires)
	if ttl <= 0 {
		return
	}
	b, err := json.Marshal(st)
	if err == nil {
		err = h.states.PutState(context.WithoutCancel(ctx), prefix+state, b, ttl)
	}
	if err != nil {
		slog.Error("github app manifest: restore state", "name", st.Name, "err", err)
	}
}

// takeState consumes state once it is confirmed issued to the caller, so a
// mismatched subject never burns it. It writes the error response itself when
// it returns false.
func (h *GitHubAppManifestHandler) takeState(w http.ResponseWriter, r *http.Request, prefix, state string) (manifestState, bool) {
	var st manifestState
	if !stateRe.MatchString(state) {
		http.Error(w, "invalid state", http.StatusBadRequest)
		return st, false
	}
	b, err := h.states.GetState(r.Context(), prefix+state)
	if err != nil {
		slog.Error("github app manifest: load state", "err", err)
		http.Error(w, "failed to load state", http.StatusInternalServerError)
		return st, false
	}
	if b == nil {
		http.Error(w, "state is unknown, expired or already used; start again", http.StatusBadRequest)
		return st, false
	}
	if err := json.Unmarshal(b, &st); err != nil {
		slog.Error("github app manifest: decode state", "err", err)
		http.Error(w, "failed to load state", http.StatusInternalServerError)
		return st, false
	}
	if st.Subject != auth.Subject(r.Context()) {
		http.Error(w, "state was issued to a different user", http.StatusForbidden)
		return st, false
	}
	taken, err := h.states.TakeStateIf(r.Context(), prefix+state, b)
	if err != nil {
		slog.Error("github app manifest: consume state", "err", err)
		http.Error(w, "failed to load state", http.StatusInternalServerError)
		return st, false
	}
	if !taken {
		http.Error(w, "state is unknown, expired or already used; start again", http.StatusBadRequest)
		return st, false
	}
	return st, true
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := decodeOne(dec, v); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// nameFree answers 409 (or 500) and returns false unless no app is called name.
func (h *GitHubAppManifestHandler) nameFree(w http.ResponseWriter, r *http.Request, name string) bool {
	_, err := h.store.GetGitHubApp(r.Context(), name)
	switch {
	case errors.Is(err, database.ErrGitHubAppNotFound):
		return true
	case err != nil:
		slog.Error("github app manifest: get app", "name", name, "err", err)
		http.Error(w, "failed to load github app", http.StatusInternalServerError)
	default:
		http.Error(w, fmt.Sprintf("github app %q already exists", name), http.StatusConflict)
	}
	return false
}

type manifestStartRequest struct {
	Name          string `json:"name"`
	Owner         string `json:"owner"`
	GitHubHost    string `json:"github_host"`
	GitHubAppName string `json:"github_app_name"`
}

// GitHubAppManifest is the manifest GitHub's app creation form accepts.
type GitHubAppManifest struct {
	Name                  string            `json:"name"`
	URL                   string            `json:"url"`
	RedirectURL           string            `json:"redirect_url"`
	SetupURL              string            `json:"setup_url"`
	Public                bool              `json:"public"`
	HookAttributes        map[string]bool   `json:"hook_attributes"`
	DefaultPermissions    map[string]string `json:"default_permissions"`
	DefaultEvents         []string          `json:"default_events"`
	RequestOAuthOnInstall bool              `json:"request_oauth_on_install"`
}

type manifestStartResponse struct {
	PostURL  string            `json:"post_url"`
	Manifest GitHubAppManifest `json:"manifest"`
	State    string            `json:"state"`
}

func validGitHubDisplayName(n string) error {
	if n == "" || utf8.RuneCountInString(n) > maxGitHubAppNameLen || !utf8.ValidString(n) || strings.ContainsFunc(n, unicode.IsControl) {
		return fmt.Errorf("github_app_name must be 1-%d characters", maxGitHubAppNameLen)
	}
	return nil
}

func defaultGitHubDisplayName(name string) string {
	n := "artifactapi-" + name
	if len(n) > maxGitHubAppNameLen {
		n = strings.TrimRight(n[:maxGitHubAppNameLen], "-.")
	}
	return n
}

func (h *GitHubAppManifestHandler) start(w http.ResponseWriter, r *http.Request) {
	var req manifestStartRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if err := validGitHubAppName(req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Owner != "" && !githubLoginRe.MatchString(req.Owner) {
		http.Error(w, fmt.Sprintf("invalid owner %q: want a GitHub organization login", req.Owner), http.StatusBadRequest)
		return
	}
	if req.GitHubAppName == "" {
		req.GitHubAppName = defaultGitHubDisplayName(req.Name)
	}
	if err := validGitHubDisplayName(req.GitHubAppName); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	webBase, apiBase, err := githubEndpoints(req.GitHubHost, h.apiHosts)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	base, ok := requrl.BaseURL(w, r)
	if !ok || !h.nameFree(w, r, req.Name) {
		return
	}
	state, err := h.putState(r.Context(), createStatePrefix, manifestState{
		Name: req.Name, Owner: req.Owner, WebBase: webBase, APIBase: apiBase, Subject: auth.Subject(r.Context()),
	})
	if err != nil {
		slog.Error("github app manifest: store state", "err", err)
		http.Error(w, "failed to store state", http.StatusInternalServerError)
		return
	}
	postURL := webBase + "/settings/apps/new"
	if req.Owner != "" {
		postURL = webBase + "/organizations/" + req.Owner + "/settings/apps/new"
	}
	ui := base + UIPath
	writeJSON(w, http.StatusOK, manifestStartResponse{
		PostURL: postURL + "?state=" + url.QueryEscape(state),
		State:   state,
		Manifest: GitHubAppManifest{
			Name:                  req.GitHubAppName,
			URL:                   ui + "/github-apps",
			RedirectURL:           ui + "/github-apps/manifest/callback",
			SetupURL:              ui + "/github-apps/manifest/installed",
			HookAttributes:        map[string]bool{"active": false},
			DefaultPermissions:    map[string]string{"contents": "read", "metadata": "read"},
			DefaultEvents:         []string{},
			RequestOAuthOnInstall: false,
		},
	})
}

type manifestConvertResponse struct {
	Name       string `json:"name"`
	AppID      int64  `json:"app_id"`
	Slug       string `json:"slug"`
	InstallURL string `json:"install_url"`
}

func installURL(webBase, slug, state string) string {
	path := "/github-apps/"
	if webBase == "https://github.com" {
		path = "/apps/"
	}
	return webBase + path + url.PathEscape(slug) + "/installations/new?state=" + url.QueryEscape(state)
}

func (h *GitHubAppManifestHandler) convert(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if !manifestCode.MatchString(req.Code) {
		http.Error(w, "invalid code", http.StatusBadRequest)
		return
	}
	st, ok := h.takeState(w, r, createStatePrefix, req.State)
	if !ok || !h.nameFree(w, r, st.Name) {
		return
	}
	conv, err := githubauth.ConvertManifest(r.Context(), h.client, st.APIBase, req.Code)
	if err != nil {
		slog.Warn("github app manifest: conversion failed", "name", st.Name, "err", err)
		var se *githubauth.StatusError
		if errors.As(err, &se) && se.Status == http.StatusNotFound {
			http.Error(w, "GitHub rejected the manifest code (expired or already used); start again", http.StatusBadRequest)
			return
		}
		if githubauth.Transient(err) {
			h.restoreState(r.Context(), createStatePrefix, req.State, st)
			http.Error(w, "GitHub manifest conversion failed; retry", http.StatusBadGateway)
			return
		}
		http.Error(w, "GitHub manifest conversion failed", http.StatusBadGateway)
		return
	}
	app := models.GitHubApp{
		Name:        st.Name,
		AppID:       conv.ID,
		APIBaseURL:  st.APIBase,
		Description: "GitHub App " + conv.Slug + " (created from manifest)",
		PrivateKey:  conv.PEM,
	}
	err = h.store.CreateGitHubApp(r.Context(), &app)
	if errors.Is(err, database.ErrGitHubAppExists) {
		slog.Warn("github app manifest: name taken after conversion; delete the orphaned GitHub App", "name", st.Name, "slug", conv.Slug)
		http.Error(w, fmt.Sprintf("github app %q already exists; delete GitHub App %s and start again", st.Name, conv.Slug), http.StatusConflict)
		return
	}
	if err != nil {
		slog.Error("github app manifest: store app", "name", st.Name, "slug", conv.Slug, "err", err)
		http.Error(w, "failed to store github app", http.StatusInternalServerError)
		return
	}
	slog.Info("github app created from manifest", "name", st.Name, "app_id", conv.ID, "slug", conv.Slug)
	st.AppID = conv.ID
	installState, err := h.issueInstallState(r.Context(), st)
	if err != nil {
		slog.Error("github app manifest: store install state", "name", st.Name, "err", err)
		http.Error(w, fmt.Sprintf("github app %q was created but its install link could not be issued; "+
			"use Install on the GitHub Apps page (POST /api/v2/github-apps/%s/install-url) to finish", st.Name, st.Name),
			http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, manifestConvertResponse{
		Name: st.Name, AppID: conv.ID, Slug: conv.Slug, InstallURL: installURL(st.WebBase, conv.Slug, installState),
	})
}

func (h *GitHubAppManifestHandler) installed(w http.ResponseWriter, r *http.Request) {
	var req struct {
		InstallationID int64  `json:"installation_id"`
		State          string `json:"state"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.InstallationID <= 0 {
		http.Error(w, "installation_id is required and must be > 0", http.StatusBadRequest)
		return
	}
	st, ok := h.takeState(w, r, installStatePrefix, req.State)
	if !ok {
		return
	}
	app, err := h.store.GetGitHubApp(r.Context(), st.Name)
	if errors.Is(err, database.ErrGitHubAppNotFound) {
		http.Error(w, fmt.Sprintf("github app %q not found", st.Name), http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("github app manifest: get app", "name", st.Name, "err", err)
		http.Error(w, "failed to load github app", http.StatusInternalServerError)
		return
	}
	if app.AppID != st.AppID {
		http.Error(w, fmt.Sprintf("github app %q was replaced since it was created", st.Name), http.StatusConflict)
		return
	}
	key, err := githubauth.ParsePrivateKey(app.PrivateKey)
	if err != nil {
		slog.Error("github app manifest: stored key", "name", st.Name, "err", err)
		http.Error(w, "stored private key is invalid", http.StatusInternalServerError)
		return
	}
	owner, err := githubauth.InstallationAppID(r.Context(), h.client, app.APIBaseURL, app.AppID, key, req.InstallationID)
	var se *githubauth.StatusError
	switch {
	case errors.As(err, &se) && se.Status == http.StatusNotFound:
		http.Error(w, fmt.Sprintf("installation %d does not belong to this app", req.InstallationID), http.StatusBadRequest)
		return
	case githubauth.Transient(err):
		slog.Warn("github app manifest: installation lookup failed", "name", st.Name, "err", err)
		h.restoreState(r.Context(), installStatePrefix, req.State, st)
		http.Error(w, "GitHub installation lookup failed; retry", http.StatusBadGateway)
		return
	case err != nil:
		slog.Warn("github app manifest: installation lookup failed", "name", st.Name, "err", err)
		http.Error(w, "GitHub installation lookup failed", http.StatusBadGateway)
		return
	case owner != app.AppID:
		http.Error(w, fmt.Sprintf("installation %d belongs to app %d, not %d", req.InstallationID, owner, app.AppID), http.StatusBadRequest)
		return
	}
	saved, err := h.store.SetGitHubAppInstallation(r.Context(), st.Name, st.AppID, req.InstallationID)
	if errors.Is(err, database.ErrGitHubAppChanged) {
		http.Error(w, fmt.Sprintf("github app %q was deleted or replaced since it was created", st.Name), http.StatusConflict)
		return
	}
	if err != nil {
		slog.Error("github app manifest: save installation", "name", st.Name, "err", err)
		http.Error(w, "failed to store github app", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// installLink issues a fresh install URL for an existing, not yet installed app.
func (h *GitHubAppManifestHandler) installLink(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := validGitHubAppName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	app, err := h.store.GetGitHubApp(r.Context(), name)
	if errors.Is(err, database.ErrGitHubAppNotFound) {
		http.Error(w, fmt.Sprintf("github app %q not found", name), http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("github app manifest: get app", "name", name, "err", err)
		http.Error(w, "failed to load github app", http.StatusInternalServerError)
		return
	}
	if app.InstallationID != 0 {
		http.Error(w, fmt.Sprintf("github app %q is already installed", name), http.StatusConflict)
		return
	}
	key, err := githubauth.ParsePrivateKey(app.PrivateKey)
	if err != nil {
		slog.Error("github app manifest: stored key", "name", name, "err", err)
		http.Error(w, "stored private key is invalid", http.StatusInternalServerError)
		return
	}
	slug, err := githubauth.AppSlug(r.Context(), h.client, app.APIBaseURL, app.AppID, key)
	if err != nil {
		slog.Warn("github app manifest: app lookup failed", "name", name, "err", err)
		http.Error(w, "GitHub app lookup failed", http.StatusBadGateway)
		return
	}
	webBase := webBaseFor(app.APIBaseURL)
	state, err := h.issueInstallState(r.Context(), manifestState{
		Name: name, WebBase: webBase, APIBase: app.APIBaseURL, Subject: auth.Subject(r.Context()), AppID: app.AppID,
	})
	if err != nil {
		slog.Error("github app manifest: store install state", "name", name, "err", err)
		http.Error(w, "failed to store state", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, manifestConvertResponse{
		Name: name, AppID: app.AppID, Slug: slug, InstallURL: installURL(webBase, slug, state),
	})
}
