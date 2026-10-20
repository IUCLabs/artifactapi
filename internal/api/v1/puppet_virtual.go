package v1

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/IUCLabs/artifactapi/internal/provider/puppet"
	"github.com/IUCLabs/artifactapi/internal/virtual"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// serveVirtualPuppet serves the merged Forge v3 module and release listings
// and 302s release details and downloads to the member owning that release.
func (h *ProxyHandler) serveVirtualPuppet(w http.ResponseWriter, r *http.Request, virt models.Virtual, path, proxyBaseURL string) {
	var doc any
	var err error
	switch {
	case path == "v3/releases":
		q := r.URL.Query()
		slug := puppet.NormalizeSlug(q.Get("module"))
		if !puppet.ValidSlug(slug) {
			forgeError(w, http.StatusBadRequest, "module is required")
			return
		}
		var d *virtual.PuppetDoc
		d, err = h.virtualEngine.PuppetReleases(r.Context(), virt, slug)
		releases := []any{}
		if errors.Is(err, virtual.ErrNotFound) {
			err = nil
		} else if err == nil {
			for _, rel := range d.Releases {
				releases = append(releases, rel)
			}
		}
		doc = puppet.ReleasesPage(slug, releases, q)
	case strings.HasPrefix(path, "v3/modules/"):
		slug := puppet.NormalizeSlug(strings.TrimPrefix(path, "v3/modules/"))
		if !puppet.ValidSlug(slug) {
			puppet.NotFound(w)
			return
		}
		var d *virtual.PuppetDoc
		if d, err = h.virtualEngine.PuppetReleases(r.Context(), virt, slug); err == nil {
			doc, err = virtual.PuppetModule(d)
		}
	default:
		var loc string
		loc, err = h.virtualEngine.PuppetRedirect(r.Context(), virt, path, proxyBaseURL)
		if err == nil {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Vary", "Host, X-Forwarded-Proto")
			http.Redirect(w, r, loc, http.StatusFound)
			return
		}
	}
	switch {
	case errors.Is(err, virtual.ErrNotFound):
		puppet.NotFound(w)
	case errors.Is(err, virtual.ErrBadPath):
		forgeError(w, http.StatusBadRequest, err.Error())
	case err != nil:
		slog.Error("virtual puppet fetch failed", "virtual", virt.Name, "path", path, "error", err)
		forgeError(w, http.StatusBadGateway, "bad gateway")
	default:
		w.Header().Set("X-Artifact-Source", "virtual")
		puppet.WriteJSON(w, doc)
	}
}

func forgeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"message": http.StatusText(code), "errors": []string{msg}})
}
