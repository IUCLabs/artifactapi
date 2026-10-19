package v1

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/IUCLabs/artifactapi/internal/virtual"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// serveVirtualGoProxy serves the merged @v/list and 302s @latest and version
// files to the member owning that version.
func (h *ProxyHandler) serveVirtualGoProxy(w http.ResponseWriter, r *http.Request, virt models.Virtual, path string) {
	body, loc, err := h.virtualEngine.GoProxy(r.Context(), virt, path, r.URL.RawQuery)
	switch {
	case errors.Is(err, virtual.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, virtual.ErrBadPath):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case err != nil:
		slog.Error("virtual goproxy fetch failed", "virtual", virt.Name, "path", path, "error", err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
	case loc != "":
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, loc, http.StatusFound)
	default:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Artifact-Source", "virtual")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}
