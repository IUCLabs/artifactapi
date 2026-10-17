// Package terraform serves local terraform repos as a real Terraform provider
// registry: service discovery, version listing, and GPG-signed downloads, so
// `terraform init` installs from a bare source address with no client config.
package terraform

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	v1 "github.com/IUCLabs/artifactapi/internal/api/v1"
	"github.com/IUCLabs/artifactapi/internal/database"
	tfprov "github.com/IUCLabs/artifactapi/internal/provider/terraform"
	"github.com/IUCLabs/artifactapi/internal/tfsign"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// ProvidersV1Path is the base the service-discovery document advertises (Terraform
// appends "{namespace}/{type}/versions" etc). MountPath is the same prefix without
// the trailing slash, for chi.Mount.
const (
	ProvidersV1Path = "/terraform/v1/providers/"
	MountPath       = "/terraform/v1/providers"
)

type Handler struct {
	db        *database.DB
	signer    *tfsign.Signer
	protocols []string
}

func NewHandler(db *database.DB, signer *tfsign.Signer, protocols string) *Handler {
	var protos []string
	for _, p := range strings.Split(protocols, ",") {
		if p = strings.TrimSpace(p); p != "" {
			protos = append(protos, p)
		}
	}
	if len(protos) == 0 {
		protos = []string{"5.0", "6.0"}
	}
	return &Handler{db: db, signer: signer, protocols: protos}
}

// Enabled reports whether a signing key is configured. Without one the registry
// cannot produce the signed SHA256SUMS the protocol requires, so it stays off.
func (h *Handler) Enabled() bool { return h.signer != nil }

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/{namespace}/{type}/versions", h.versions)
	r.Get("/{namespace}/{type}/{version}/download/{os}/{arch}", h.download)
	r.Get("/{namespace}/{type}/{version}/sha256sums", h.sha256sums)
	r.Get("/{namespace}/{type}/{version}/sha256sums.sig", h.sha256sumsSig)
	return r
}

// ServiceDiscovery answers /.well-known/terraform.json, pointing Terraform at the
// providers.v1 protocol base.
func (h *Handler) ServiceDiscovery(w http.ResponseWriter, r *http.Request) {
	if !h.Enabled() {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, map[string]string{"providers.v1": ProvidersV1Path})
}

// providerFile is one resolved platform artifact within a repo.
type providerFile struct {
	version  string
	os       string
	arch     string
	filePath string // path within the repo, e.g. myorg/artifactapi/...zip
	sha256   string // hex, no "sha256:" prefix
}

// resolve finds every provider zip of the given type in the repo (namespace).
// The Terraform source namespace maps to the artifactapi repo name; the provider
// is matched by type across whatever in-repo folder it was uploaded under.
func (h *Handler) resolve(r *http.Request, namespace, typeName string) ([]providerFile, error) {
	remote, err := h.db.GetRemote(r.Context(), namespace)
	if err != nil || remote.PackageType != models.PackageTerraform {
		return nil, nil
	}

	rows, err := h.db.ListLocalFiles(r.Context(), namespace, 10000, 0)
	if err != nil {
		return nil, err
	}

	var out []providerFile
	for _, row := range rows {
		parsed := tfprov.ParseProviderZip(path.Base(row.FilePath))
		if !parsed.Ok || parsed.Type != typeName {
			continue
		}
		out = append(out, providerFile{
			version:  parsed.Version,
			os:       parsed.OS,
			arch:     parsed.Arch,
			filePath: row.FilePath,
			sha256:   strings.TrimPrefix(row.ContentHash, "sha256:"),
		})
	}
	return out, nil
}

func (h *Handler) versions(w http.ResponseWriter, r *http.Request) {
	if !h.Enabled() {
		http.NotFound(w, r)
		return
	}
	namespace := chi.URLParam(r, "namespace")
	typeName := chi.URLParam(r, "type")

	files, err := h.resolve(r, namespace, typeName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(files) == 0 {
		http.NotFound(w, r)
		return
	}

	// Group platforms by version, de-duplicated and stably ordered.
	type platform struct {
		OS   string `json:"os"`
		Arch string `json:"arch"`
	}
	platforms := map[string]map[string]platform{}
	for _, f := range files {
		if platforms[f.version] == nil {
			platforms[f.version] = map[string]platform{}
		}
		platforms[f.version][f.os+"_"+f.arch] = platform{OS: f.os, Arch: f.arch}
	}

	type versionEntry struct {
		Version   string     `json:"version"`
		Protocols []string   `json:"protocols"`
		Platforms []platform `json:"platforms"`
	}
	out := struct {
		Versions []versionEntry `json:"versions"`
	}{}
	for version, plats := range platforms {
		entry := versionEntry{Version: version, Protocols: h.protocols}
		for _, p := range plats {
			entry.Platforms = append(entry.Platforms, p)
		}
		sort.Slice(entry.Platforms, func(i, j int) bool {
			return entry.Platforms[i].OS+entry.Platforms[i].Arch < entry.Platforms[j].OS+entry.Platforms[j].Arch
		})
		out.Versions = append(out.Versions, entry)
	}
	sort.Slice(out.Versions, func(i, j int) bool { return out.Versions[i].Version < out.Versions[j].Version })

	writeJSON(w, out)
}

func (h *Handler) download(w http.ResponseWriter, r *http.Request) {
	if !h.Enabled() {
		http.NotFound(w, r)
		return
	}
	namespace := chi.URLParam(r, "namespace")
	typeName := chi.URLParam(r, "type")
	version := chi.URLParam(r, "version")
	osName := chi.URLParam(r, "os")
	arch := chi.URLParam(r, "arch")

	files, err := h.resolve(r, namespace, typeName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var match *providerFile
	for i := range files {
		if files[i].version == version && files[i].os == osName && files[i].arch == arch {
			match = &files[i]
			break
		}
	}
	if match == nil {
		http.NotFound(w, r)
		return
	}

	base := baseURL(r)
	verBase := fmt.Sprintf("%s%s/%s/%s", base+ProvidersV1Path, namespace, typeName, version)

	type gpgKey struct {
		KeyID      string `json:"key_id"`
		ASCIIArmor string `json:"ascii_armor"`
	}
	resp := struct {
		Protocols           []string `json:"protocols"`
		OS                  string   `json:"os"`
		Arch                string   `json:"arch"`
		Filename            string   `json:"filename"`
		DownloadURL         string   `json:"download_url"`
		SHASumsURL          string   `json:"shasums_url"`
		SHASumsSignatureURL string   `json:"shasums_signature_url"`
		SHASum              string   `json:"shasum"`
		SigningKeys         struct {
			GPGPublicKeys []gpgKey `json:"gpg_public_keys"`
		} `json:"signing_keys"`
	}{
		Protocols:           h.protocols,
		OS:                  match.os,
		Arch:                match.arch,
		Filename:            path.Base(match.filePath),
		DownloadURL:         fmt.Sprintf("%s/api/v1/local/%s/%s", base, namespace, match.filePath),
		SHASumsURL:          verBase + "/sha256sums",
		SHASumsSignatureURL: verBase + "/sha256sums.sig",
		SHASum:              match.sha256,
	}
	resp.SigningKeys.GPGPublicKeys = []gpgKey{{
		KeyID:      h.signer.KeyID(),
		ASCIIArmor: h.signer.PublicKeyArmor(),
	}}

	writeJSON(w, resp)
}

func (h *Handler) sha256sums(w http.ResponseWriter, r *http.Request) {
	sums, ok := h.buildSums(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(sums)
}

func (h *Handler) sha256sumsSig(w http.ResponseWriter, r *http.Request) {
	sums, ok := h.buildSums(w, r)
	if !ok {
		return
	}
	sig, err := h.signer.Sign(sums)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(sig)
}

// buildSums renders the SHA256SUMS body for one version: one "<hex>  <filename>"
// line per platform zip, sorted by filename so the signed bytes are stable.
func (h *Handler) buildSums(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if !h.Enabled() {
		http.NotFound(w, r)
		return nil, false
	}
	namespace := chi.URLParam(r, "namespace")
	typeName := chi.URLParam(r, "type")
	version := chi.URLParam(r, "version")

	files, err := h.resolve(r, namespace, typeName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, false
	}

	var lines []string
	for _, f := range files {
		if f.version != version {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s  %s", f.sha256, path.Base(f.filePath)))
	}
	if len(lines) == 0 {
		http.NotFound(w, r)
		return nil, false
	}
	sort.Strings(lines)
	return []byte(strings.Join(lines, "\n") + "\n"), true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func baseURL(r *http.Request) string {
	return v1.Scheme(r) + "://" + r.Host
}
