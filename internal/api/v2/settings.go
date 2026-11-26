package v2

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/IUCLabs/artifactapi/internal/proxy"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// SettingsStore persists the settings overrides. *database.DB satisfies it.
type SettingsStore interface {
	GetSettings(ctx context.Context) (models.Settings, error)
	PutSettings(ctx context.Context, s models.Settings) error
	ResetSettings(ctx context.Context) error
}

// SettingsHandler serves the singleton runtime settings; reads return effective values.
type SettingsHandler struct {
	store    SettingsStore
	defaults proxy.ProbeConfig
}

func NewSettingsHandler(store SettingsStore, defaults proxy.ProbeConfig) *SettingsHandler {
	return &SettingsHandler{store: store, defaults: defaults}
}

func (h *SettingsHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.get)
	r.Put("/", h.put)
	r.Delete("/", h.reset)
	return r
}

func (h *SettingsHandler) get(w http.ResponseWriter, r *http.Request) {
	s, err := h.store.GetSettings(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, h.defaults.Apply(s).Settings())
}

// put replaces every override; an omitted or null field reverts to its env default.
func (h *SettingsHandler) put(w http.ResponseWriter, r *http.Request) {
	var s models.Settings
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if err := s.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	effective := h.defaults.Apply(s)
	if err := effective.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.store.PutSettings(r.Context(), s); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, effective.Settings())
}

func (h *SettingsHandler) reset(w http.ResponseWriter, r *http.Request) {
	if err := h.store.ResetSettings(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
