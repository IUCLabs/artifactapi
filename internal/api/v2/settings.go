package v2

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

// SettingsStore persists the global settings overrides. *database.DB satisfies it.
type SettingsStore interface {
	GetSettings(ctx context.Context) (models.Settings, error)
	PutSettings(ctx context.Context, s models.Settings) error
	ResetSettings(ctx context.Context) error
}

// SettingsHandler serves the singleton global settings; responses carry effective values.
type SettingsHandler struct {
	store    SettingsStore
	defaults models.Settings
}

// NewSettingsHandler takes the env defaults with every field set.
func NewSettingsHandler(store SettingsStore, defaults models.Settings) *SettingsHandler {
	return &SettingsHandler{store: store, defaults: defaults}
}

func (h *SettingsHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.get)
	r.Put("/", h.put)
	r.Patch("/", h.patch)
	r.Delete("/", h.reset)
	return r
}

func (h *SettingsHandler) get(w http.ResponseWriter, r *http.Request) {
	s, err := h.store.GetSettings(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, h.defaults.Overlay(s))
}

// put replaces every override; an omitted or null field reverts to its env default.
func (h *SettingsHandler) put(w http.ResponseWriter, r *http.Request) {
	var s models.Settings
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	h.save(w, r, s)
}

// patch overrides only the fields set in the body, keeping the rest.
func (h *SettingsHandler) patch(w http.ResponseWriter, r *http.Request) {
	var body models.Settings
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	stored, err := h.store.GetSettings(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.save(w, r, stored.Overlay(body))
}

func (h *SettingsHandler) save(w http.ResponseWriter, r *http.Request, s models.Settings) {
	effective := h.defaults.Overlay(s)
	if err := effective.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.store.PutSettings(r.Context(), s); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, effective)
}

func (h *SettingsHandler) reset(w http.ResponseWriter, r *http.Request) {
	if err := h.store.ResetSettings(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
