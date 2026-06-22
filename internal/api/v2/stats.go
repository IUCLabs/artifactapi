package v2

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/IUCLabs/artifactapi/internal/database"
)

type StatsHandler struct {
	db *database.DB
}

func NewStatsHandler(db *database.DB) *StatsHandler {
	return &StatsHandler{db: db}
}

func (h *StatsHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.overview)
	r.Get("/top-remotes", h.topRemotes)
	r.Get("/top-files-by-hits", h.topFilesByHits)
	r.Get("/top-files-by-bandwidth", h.topFilesByBandwidth)
	return r
}

func (h *StatsHandler) overview(w http.ResponseWriter, r *http.Request) {
	stats, err := h.db.GetOverviewStats(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (h *StatsHandler) topRemotes(w http.ResponseWriter, r *http.Request) {
	remotes, err := h.db.GetTopRemotes(r.Context(), 10)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, remotes)
}

func (h *StatsHandler) topFilesByHits(w http.ResponseWriter, r *http.Request) {
	files, err := h.db.GetTopFilesByHits(r.Context(), 10)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, files)
}

func (h *StatsHandler) topFilesByBandwidth(w http.ResponseWriter, r *http.Request) {
	files, err := h.db.GetTopFilesByBandwidth(r.Context(), 10)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, files)
}
