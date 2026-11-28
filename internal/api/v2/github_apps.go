package v2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"

	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/githubauth"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// GitHubAppStore persists GitHub Apps. *database.DB satisfies it.
type GitHubAppStore interface {
	ListGitHubApps(ctx context.Context) ([]models.GitHubApp, error)
	GetGitHubApp(ctx context.Context, name string) (*models.GitHubApp, error)
	PutGitHubApp(ctx context.Context, a *models.GitHubApp) error
	DeleteGitHubApp(ctx context.Context, name string) error
}

// GitHubAppsHandler serves /api/v2/github-apps. Private keys are write-only.
type GitHubAppsHandler struct {
	store    GitHubAppStore
	apiHosts []string
}

// NewGitHubAppsHandler serves apps whose api_base_url is api.github.com or one
// of apiHosts.
func NewGitHubAppsHandler(store GitHubAppStore, apiHosts []string) *GitHubAppsHandler {
	return &GitHubAppsHandler{store: store, apiHosts: apiHosts}
}

func (h *GitHubAppsHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.list)
	r.Get("/{name}", h.get)
	r.Put("/{name}", h.put)
	r.Delete("/{name}", h.del)
	return r
}

var githubAppNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func validGitHubAppName(name string) error {
	if !githubAppNameRe.MatchString(name) {
		return fmt.Errorf("invalid github app name %q: must match %s", name, githubAppNameRe)
	}
	return nil
}

func (h *GitHubAppsHandler) list(w http.ResponseWriter, r *http.Request) {
	apps, err := h.store.ListGitHubApps(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, apps)
}

func (h *GitHubAppsHandler) get(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	app, err := h.store.GetGitHubApp(r.Context(), name)
	if errors.Is(err, database.ErrGitHubAppNotFound) {
		http.Error(w, fmt.Sprintf("github app %q not found", name), http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, app)
}

func (h *GitHubAppsHandler) put(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := validGitHubAppName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var spec models.GitHubAppSpec
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := decodeOne(dec, &spec); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := spec.Validate(h.apiHosts); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := githubauth.ParsePrivateKey(spec.PrivateKey); err != nil {
		http.Error(w, "invalid private_key: "+err.Error(), http.StatusBadRequest)
		return
	}
	app := models.GitHubApp{
		Name:           name,
		AppID:          spec.AppID,
		InstallationID: spec.InstallationID,
		APIBaseURL:     spec.APIBaseURL,
		Description:    spec.Description,
		PrivateKey:     spec.PrivateKey,
	}
	if err := h.store.PutGitHubApp(r.Context(), &app); err != nil {
		slog.Error("put github app", "name", name, "err", err)
		http.Error(w, "failed to store github app", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, app)
}

func (h *GitHubAppsHandler) del(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	err := h.store.DeleteGitHubApp(r.Context(), name)
	var inUse *database.GitHubAppInUseError
	switch {
	case errors.Is(err, database.ErrGitHubAppNotFound):
		http.Error(w, fmt.Sprintf("github app %q not found", name), http.StatusNotFound)
	case errors.As(err, &inUse):
		http.Error(w, inUse.Error(), http.StatusConflict)
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
