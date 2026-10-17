package v2

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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

// MetadataFlusher purges a remote's cached mutable metadata (repodata / Release
// / APKINDEX freshness keys). *cache.Redis satisfies it.
type MetadataFlusher interface {
	FlushRemote(ctx context.Context, remote string) error
}

type RemotesHandler struct {
	db      *database.DB
	cache   MetadataFlusher
	primers map[models.PackageType]Primer
}

// NewRemotesHandler wires the handler to the metadata cache and per-type
// primers. cache may be nil (flush-on-backend-change is skipped); primers may
// be nil (a package type with no registered primer simply skips priming).
func NewRemotesHandler(db *database.DB, cache MetadataFlusher, primers map[models.PackageType]Primer) *RemotesHandler {
	return &RemotesHandler{db: db, cache: cache, primers: primers}
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

// defaultMutableTTL matches the remotes.mutable_ttl column default; a create
// that omits mutable_ttl gets it, an explicit 0 still means never expire.
const defaultMutableTTL = 3600

func (h *RemotesHandler) create(w http.ResponseWriter, r *http.Request) {
	remote := models.Remote{MutableTTL: defaultMutableTTL}
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
	if err := remote.ValidateMirrorlist(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := remote.ValidateMirrorStrategy(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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
	// Capture the current backend before the update so we can tell whether the
	// remote's base_url (its upstream) changed. A read failure just means we
	// skip the freshness flush; it must not block the update.
	existing, err := h.db.GetRemote(r.Context(), name)
	oldKnown := err == nil
	var remote models.Remote
	if oldKnown {
		remote.MutableTTL = existing.MutableTTL
	}
	if err := json.NewDecoder(r.Body).Decode(&remote); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	remote.Name = name
	if err := remote.ValidateMirrorlist(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := remote.ValidateMirrorStrategy(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := remote.ValidatePatterns(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.db.UpdateRemote(r.Context(), &remote); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Changing the backend invalidates any cached mutable metadata (repodata /
	// Release / APKINDEX): purge it so the next request re-fetches from the new
	// upstream instead of serving stale data until TTL expiry. A flush failure
	// is logged but does not fail the request — the DB update already landed.
	if oldKnown && existing.BaseURL != remote.BaseURL && h.cache != nil {
		if err := h.cache.FlushRemote(r.Context(), name); err != nil {
			slog.Warn("flush cached metadata after base_url change failed",
				"remote", name, "error", err)
		} else {
			slog.Info("flushed cached metadata after base_url change",
				"remote", name, "old_base_url", existing.BaseURL, "new_base_url", remote.BaseURL)
		}
	}
	writeJSON(w, http.StatusOK, remote)
}

func (h *RemotesHandler) del(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := h.db.DeleteRemote(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if h.cache != nil {
		if err := h.cache.FlushRemote(r.Context(), name); err != nil {
			slog.Warn("flush cached metadata after delete failed", "remote", name, "error", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
