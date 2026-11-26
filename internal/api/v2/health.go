package v2

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/IUCLabs/artifactapi/internal/cache"
	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/storage"
)

type HealthHandler struct {
	db    *database.DB
	cache *cache.Redis
	store *storage.S3
}

func NewHealthHandler(db *database.DB, c *cache.Redis, s *storage.S3) *HealthHandler {
	return &HealthHandler{db: db, cache: c, store: s}
}

func (h *HealthHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.health)
	return r
}

func (h *HealthHandler) health(w http.ResponseWriter, r *http.Request) {
	status := map[string]string{
		"status":   "ok",
		"postgres": "ok",
		"redis":    "ok",
		"s3":       "ok",
	}

	if err := h.db.Pool.Ping(r.Context()); err != nil {
		status["postgres"] = "error"
		status["status"] = "degraded"
	}

	switch h.cache.StoreHealth(r.Context()) {
	case cache.StoreStateDown:
		status["s3"] = "down"
		status["status"] = "degraded"
	case cache.StoreStateUnknown:
		status["s3"] = "unknown"
	}

	writeJSON(w, http.StatusOK, status)
}
