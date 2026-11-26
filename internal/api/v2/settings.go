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
	UpdateSettings(ctx context.Context, fn func(models.Settings) (models.Settings, error)) error
	ResetSettings(ctx context.Context) error
}

// SettingsHandler serves the singleton global settings; responses carry effective values.
type SettingsHandler struct {
	store    SettingsStore
	defaults models.Settings
}

// settingsResponse is the effective settings plus the stored overrides (null when unset);
// Valid is false, with Error saying why, when the effective settings fail validation, in
// which case the probe keeps its last valid settings.
type settingsResponse struct {
	models.Settings
	Overrides models.Settings `json:"overrides"`
	Valid     bool            `json:"valid"`
	Error     string          `json:"error,omitempty"`
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

func (h *SettingsHandler) respond(w http.ResponseWriter, stored models.Settings) {
	resp := settingsResponse{Settings: h.defaults.Overlay(stored), Overrides: stored, Valid: true}
	if err := resp.Validate(); err != nil {
		resp.Valid, resp.Error = false, err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *SettingsHandler) get(w http.ResponseWriter, r *http.Request) {
	s, err := h.store.GetSettings(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.respond(w, s)
}

// put replaces every override; an omitted or null field reverts to its env default.
func (h *SettingsHandler) put(w http.ResponseWriter, r *http.Request) {
	var s models.Settings
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.validate(s); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.store.PutSettings(r.Context(), s); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.respond(w, s)
}

// patch is a JSON merge patch: a value overrides its field, null reverts it to the env
// default, and an absent field is kept.
func (h *SettingsHandler) patch(w http.ResponseWriter, r *http.Request) {
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	var saved models.Settings
	var invalid error
	err := h.store.UpdateSettings(r.Context(), func(stored models.Settings) (models.Settings, error) {
		next, perr := stored.Patch(raw)
		if perr == nil {
			perr = h.validate(next)
		}
		if invalid = perr; invalid != nil {
			return stored, invalid
		}
		saved = next
		return next, nil
	})
	switch {
	case invalid != nil:
		http.Error(w, invalid.Error(), http.StatusBadRequest)
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		h.respond(w, saved)
	}
}

func (h *SettingsHandler) validate(s models.Settings) error {
	return h.defaults.Overlay(s).Validate()
}

func (h *SettingsHandler) reset(w http.ResponseWriter, r *http.Request) {
	if err := h.store.ResetSettings(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
