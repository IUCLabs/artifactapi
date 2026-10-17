package v1

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/IUCLabs/artifactapi/internal/virtual"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// serveVirtualGems serves the merged compact index and 302s downloads to the
// member owning that version.
func (h *ProxyHandler) serveVirtualGems(w http.ResponseWriter, r *http.Request, virt models.Virtual, path, proxyBaseURL string) {
	var body []byte
	var loc string
	var err error
	if isGemsIndexPath(path) {
		body, _, err = h.virtualEngine.FetchGems(r.Context(), virt, path)
	} else {
		loc, err = h.virtualEngine.GemsRedirect(r.Context(), virt, path, r.URL.RawQuery, proxyBaseURL)
	}
	switch {
	case errors.Is(err, virtual.ErrNotFound):
		gemsText(w, http.StatusNotFound, "not found")
	case errors.Is(err, virtual.ErrBadPath):
		gemsText(w, http.StatusBadRequest, err.Error())
	case err != nil:
		slog.Error("virtual gems fetch failed", "virtual", virt.Name, "path", path, "error", err)
		gemsText(w, http.StatusBadGateway, "bad gateway")
	case loc != "":
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Vary", "Host, X-Forwarded-Proto")
		http.Redirect(w, r, loc, http.StatusFound)
	default:
		w.Header().Set("X-Artifact-Source", "virtual")
		gemsText(w, http.StatusOK, string(body))
	}
}
