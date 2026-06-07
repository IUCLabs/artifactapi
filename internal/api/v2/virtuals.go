package v2

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

type VirtualsHandler struct {
	db *database.DB
}

func NewVirtualsHandler(db *database.DB) *VirtualsHandler {
	return &VirtualsHandler{db: db}
}

func (h *VirtualsHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.list)
	r.Post("/", h.create)
	r.Get("/{name}", h.get)
	r.Put("/{name}", h.update)
	r.Delete("/{name}", h.del)
	return r
}

func (h *VirtualsHandler) list(w http.ResponseWriter, r *http.Request) {
	virtuals, err := h.db.ListVirtuals(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, virtuals)
}

func (h *VirtualsHandler) get(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	virt, err := h.db.GetVirtual(r.Context(), name)
	if err != nil {
		http.Error(w, fmt.Sprintf("virtual %q not found", name), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, virt)
}

func (h *VirtualsHandler) create(w http.ResponseWriter, r *http.Request) {
	var virt models.Virtual
	if err := json.NewDecoder(r.Body).Decode(&virt); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if err := h.db.CreateVirtual(r.Context(), &virt); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, virt)
}

func (h *VirtualsHandler) update(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var virt models.Virtual
	if err := json.NewDecoder(r.Body).Decode(&virt); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	virt.Name = name
	if err := h.db.UpdateVirtual(r.Context(), &virt); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, virt)
}

func (h *VirtualsHandler) del(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := h.db.DeleteVirtual(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
