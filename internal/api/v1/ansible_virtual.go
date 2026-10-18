package v1

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/IUCLabs/artifactapi/internal/provider/ansible"
	"github.com/IUCLabs/artifactapi/internal/virtual"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// serveVirtualAnsible serves the merged Galaxy v3 API and 302s downloads to
// the member owning that version.
func (h *ProxyHandler) serveVirtualAnsible(w http.ResponseWriter, r *http.Request, virt models.Virtual, path, proxyBaseURL string) {
	ap := ansible.ParseAPIPath(path)
	base := proxyBaseURL + "/api/v1/virtual/" + url.PathEscape(virt.Name)
	var doc any
	var err error
	switch ap.Kind {
	case ansible.APIRoot:
		doc = ansible.Root()
	case ansible.APIVersion:
		var body []byte
		if body, err = h.virtualEngine.AnsibleVersion(r.Context(), virt, ap.Namespace, ap.Name, ap.Version, proxyBaseURL); err == nil {
			doc = json.RawMessage(body)
		}
	case ansible.APICollection, ansible.APIVersions:
		var d *virtual.AnsibleDoc
		if d, err = h.virtualEngine.AnsibleVersions(r.Context(), virt, ap.Namespace, ap.Name); err == nil {
			if ap.Kind == ansible.APICollection {
				doc = ansible.CollectionDoc(base, ap.Namespace, ap.Name, d.Refs)
			} else {
				limit, offset := ansible.Page(r.URL.Query())
				doc = ansible.VersionsPage(base, ap.Namespace, ap.Name, d.Refs, limit, offset)
			}
		}
	default:
		loc, ok, err := h.virtualEngine.MemberRedirect(r.Context(), virt, path, r.URL.RawQuery, proxyBaseURL)
		switch {
		case err != nil:
			ansibleError(w, http.StatusBadRequest, err.Error())
		case !ok:
			ansibleError(w, http.StatusNotFound, "not found")
		default:
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Vary", "Host, X-Forwarded-Proto")
			http.Redirect(w, r, loc, http.StatusFound)
		}
		return
	}
	switch {
	case errors.Is(err, virtual.ErrNotFound):
		ansibleError(w, http.StatusNotFound, "not found")
	case err != nil:
		slog.Error("virtual ansible fetch failed", "virtual", virt.Name, "path", path, "error", err)
		ansibleError(w, http.StatusBadGateway, "bad gateway")
	default:
		w.Header().Set("X-Artifact-Source", "virtual")
		writeAnsibleJSON(w, http.StatusOK, doc)
	}
}
