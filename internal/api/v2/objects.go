package v2

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/IUCLabs/artifactapi/internal/database"
)

type ObjectsHandler struct {
	db *database.DB
}

func NewObjectsHandler(db *database.DB) *ObjectsHandler {
	return &ObjectsHandler{db: db}
}

func (h *ObjectsHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.list)
	r.Delete("/*", h.evict)
	return r
}

// LocalRoutes lists and evicts objects for local repos, which live in the
// local_files table rather than the artifacts table used by remotes.
func (h *ObjectsHandler) LocalRoutes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.listLocal)
	r.Delete("/*", h.evictLocal)
	return r
}

// pageBounds parses the shared page/per_page query params into a SQL limit and offset.
func pageBounds(r *http.Request) (limit, offset int) {
	limit, _ = strconv.Atoi(r.URL.Query().Get("per_page"))
	if limit <= 0 || limit > 5000 {
		limit = 50
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page <= 0 {
		page = 1
	}
	return limit, (page - 1) * limit
}

func (h *ObjectsHandler) list(w http.ResponseWriter, r *http.Request) {
	remoteName := chi.URLParam(r, "name")
	limit, offset := pageBounds(r)

	artifacts, err := h.db.ListArtifacts(r.Context(), remoteName, limit, offset)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, artifacts)
}

func (h *ObjectsHandler) listLocal(w http.ResponseWriter, r *http.Request) {
	repoName := chi.URLParam(r, "name")
	limit, offset := pageBounds(r)

	artifacts, err := h.db.ListLocalArtifacts(r.Context(), repoName, limit, offset)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, artifacts)
}

func (h *ObjectsHandler) evictLocal(w http.ResponseWriter, r *http.Request) {
	repoName := chi.URLParam(r, "name")
	path := chi.URLParam(r, "*")

	if err := deleteLocalFile(r.Context(), h.db, repoName, path); err != nil {
		http.Error(w, fmt.Sprintf("evict failed: %v", err), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *ObjectsHandler) evict(w http.ResponseWriter, r *http.Request) {
	remoteName := chi.URLParam(r, "name")
	path := chi.URLParam(r, "*")

	if err := h.db.DeleteArtifact(r.Context(), remoteName, path); err != nil {
		http.Error(w, fmt.Sprintf("evict failed: %v", err), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
