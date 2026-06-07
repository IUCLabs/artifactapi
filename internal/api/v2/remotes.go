package v2

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

type RemotesHandler struct {
	db *database.DB
}

func NewRemotesHandler(db *database.DB) *RemotesHandler {
	return &RemotesHandler{db: db}
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
	if err := h.db.CreateRemote(r.Context(), &remote); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
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
