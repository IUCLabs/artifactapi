package v2

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/proxy"
)

type ProbeHandler struct {
	engine *proxy.Engine
	db     *database.DB
}

func NewProbeHandler(engine *proxy.Engine, db *database.DB) *ProbeHandler {
	return &ProbeHandler{engine: engine, db: db}
}

func (h *ProbeHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.probe)
	return r
}

type probeRequest struct {
	Remote string `json:"remote"`
	Path   string `json:"path"`
}

type probeResponse struct {
	Status      int               `json:"status"`
	Source      string            `json:"source,omitempty"`
	ContentType string            `json:"content_type,omitempty"`
	SizeBytes   int64             `json:"size_bytes"`
	Headers     map[string]string `json:"headers,omitempty"`
	DurationMS  int64             `json:"duration_ms"`
	Error       string            `json:"error,omitempty"`
}

func (h *ProbeHandler) probe(w http.ResponseWriter, r *http.Request) {
	var req probeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if req.Remote == "" || req.Path == "" {
		http.Error(w, "remote and path are required", http.StatusBadRequest)
		return
	}

	remote, err := h.db.GetRemote(r.Context(), req.Remote)
	if err != nil {
		writeJSON(w, http.StatusOK, probeResponse{
			Status: 404,
			Error:  fmt.Sprintf("remote %q not found", req.Remote),
		})
		return
	}

	prov, err := provider.Get(remote.PackageType)
	if err != nil {
		writeJSON(w, http.StatusOK, probeResponse{
			Status: 500,
			Error:  fmt.Sprintf("no provider for %q", remote.PackageType),
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	start := time.Now()
	result, err := h.engine.Fetch(ctx, *remote, req.Path, prov)
	duration := time.Since(start).Milliseconds()

	if err != nil {
		writeJSON(w, http.StatusOK, probeResponse{
			Status:     502,
			DurationMS: duration,
			Error:      err.Error(),
		})
		return
	}

	io.Copy(io.Discard, result.Reader)
	result.Reader.Close()

	writeJSON(w, http.StatusOK, probeResponse{
		Status:      200,
		Source:      result.Source,
		ContentType: result.ContentType,
		SizeBytes:   result.Size,
		DurationMS:  duration,
		Headers: map[string]string{
			"X-Artifact-Source": result.Source,
			"X-Artifact-Size":   fmt.Sprintf("%d", result.Size),
			"Content-Type":      result.ContentType,
		},
	})
}
