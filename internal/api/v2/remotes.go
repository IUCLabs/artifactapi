package v2

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// Primer enqueues a background metadata prime for a newly created remote so the
// create call never blocks on a derive. *rpm.Syncer and *deb.Syncer satisfy it.
type Primer interface {
	EnqueuePrime(remote models.Remote)
}

type RemotesHandler struct {
	db      *database.DB
	primers map[models.PackageType]Primer
}

// NewRemotesHandler wires the handler to the per-type metadata primers. primers
// may be nil; a package type with no registered primer simply skips priming.
func NewRemotesHandler(db *database.DB, primers map[models.PackageType]Primer) *RemotesHandler {
	return &RemotesHandler{db: db, primers: primers}
}

func (h *RemotesHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.list)
	r.Post("/", h.create)
	r.Get("/{name}", h.get)
	r.Put("/{name}", h.update)
	r.Delete("/{name}", h.del)
	return r
}

func (h *RemotesHandler) list(w http.ResponseWriter, r *http.Request) {
	remotes, err := h.db.ListRemotes(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, remotes)
}

func (h *RemotesHandler) get(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	remote, err := h.db.GetRemote(r.Context(), name)
	if err != nil {
		http.Error(w, fmt.Sprintf("remote %q not found", name), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, remote)
}

func (h *RemotesHandler) create(w http.ResponseWriter, r *http.Request) {
	var remote models.Remote
	if err := json.NewDecoder(r.Body).Decode(&remote); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if !remote.PackageType.Valid() {
		http.Error(w, fmt.Sprintf("invalid package type: %q", remote.PackageType), http.StatusBadRequest)
		return
	}
	if remote.RepoType == "" {
		remote.RepoType = models.RepoTypeRemote
	}
	if !remote.RepoType.Valid() {
		http.Error(w, fmt.Sprintf("invalid repo type: %q", remote.RepoType), http.StatusBadRequest)
		return
	}
	if remote.RepoType == models.RepoTypeRemote && remote.BaseURL == "" {
		http.Error(w, "base_url is required for remote repositories", http.StatusBadRequest)
		return
	}
	if err := remote.ValidatePatterns(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.db.CreateRemote(r.Context(), &remote); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Prime a metadata-only remote (github_rpm/github_deb) in the background so
	// its first index request is served from cache instead of a cold derive.
	if primer := h.primers[remote.PackageType]; primer != nil {
		primer.EnqueuePrime(remote)
	}
	writeJSON(w, http.StatusCreated, remote)
}

func (h *RemotesHandler) update(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var remote models.Remote
	if err := json.NewDecoder(r.Body).Decode(&remote); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	remote.Name = name
	if err := remote.ValidatePatterns(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.db.UpdateRemote(r.Context(), &remote); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, remote)
}

func (h *RemotesHandler) del(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := h.db.DeleteRemote(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
