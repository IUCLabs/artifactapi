package v2

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/storage"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

var providerZipRe = regexp.MustCompile(
	`^terraform-provider-([a-zA-Z0-9_-]+)_([0-9]+\.[0-9]+\.[0-9]+(?:-[a-zA-Z0-9.]+)?)_([a-z0-9]+)_([a-z0-9]+)\.zip$`,
)

type LocalHandler struct {
	db    *database.DB
	store *storage.S3
	cas   *storage.CAS
}

func NewLocalHandler(db *database.DB, store *storage.S3) *LocalHandler {
	return &LocalHandler{
		db:    db,
		store: store,
		cas:   storage.NewCAS(store),
	}
}

func (h *LocalHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Put("/*", h.upload)
	r.Get("/*", h.download)
	r.Delete("/*", h.remove)
	return r
}

func (h *LocalHandler) upload(w http.ResponseWriter, r *http.Request) {
	repoName := chi.URLParam(r, "name")
	filePath := chi.URLParam(r, "*")

	if filePath == "" {
		http.Error(w, "file path required", http.StatusBadRequest)
		return
	}

	remote, err := h.db.GetRemote(r.Context(), repoName)
	if err != nil {
		http.Error(w, fmt.Sprintf("remote %q not found", repoName), http.StatusNotFound)
		return
	}
	if remote.RepoType != models.RepoTypeLocal {
		http.Error(w, "upload only allowed for local repository types", http.StatusBadRequest)
		return
	}

	if remote.PackageType == models.PackageTerraform {
		h.uploadTerraformProvider(w, r, remote, filePath)
		return
	}

	h.uploadGeneric(w, r, remote, filePath)
}

func (h *LocalHandler) uploadTerraformProvider(w http.ResponseWriter, r *http.Request, remote *models.Remote, filePath string) {
	parts := strings.Split(filePath, "/")
	if len(parts) != 3 {
		http.Error(w, "path must be {namespace}/{type}/{filename}.zip", http.StatusBadRequest)
		return
	}
	namespace, typeName, filename := parts[0], parts[1], parts[2]

	m := providerZipRe.FindStringSubmatch(filename)
	if m == nil {
		http.Error(w, fmt.Sprintf(
			"filename %q does not match terraform-provider-{type}_{version}_{os}_{arch}.zip",
			filename,
		), http.StatusBadRequest)
		return
	}
	fileType, version, os, arch := m[1], m[2], m[3], m[4]

	if fileType != typeName {
		http.Error(w, fmt.Sprintf(
			"provider type in filename %q does not match path type %q",
			fileType, typeName,
		), http.StatusBadRequest)
		return
	}

	storagePath := fmt.Sprintf("%s/%s/%s", namespace, typeName, filename)

	existing, err := h.db.GetLocalFile(r.Context(), remote.Name, storagePath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if existing != nil {
		http.Error(w, fmt.Sprintf(
			"provider %s/%s version %s for %s_%s already exists; overwrites are not allowed",
			namespace, typeName, version, os, arch,
		), http.StatusConflict)
		return
	}

	result, err := h.cas.Store(r.Context(), r.Body, "application/zip")
	if err != nil {
		http.Error(w, fmt.Sprintf("store failed: %v", err), http.StatusInternalServerError)
		return
	}

	if err := h.db.UpsertBlob(r.Context(), result.ContentHash, result.S3Key, result.SizeBytes, "application/zip"); err != nil {
		http.Error(w, fmt.Sprintf("record blob: %v", err), http.StatusInternalServerError)
		return
	}

	if err := h.db.CreateLocalFile(r.Context(), remote.Name, storagePath, result.ContentHash); err != nil {
		if errors.Is(err, database.ErrAlreadyExists) {
			http.Error(w, fmt.Sprintf("file %q already exists; overwrites are not allowed", storagePath), http.StatusConflict)
			return
		}
		http.Error(w, fmt.Sprintf("record file: %v", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"namespace":    namespace,
		"type":         typeName,
		"version":      version,
		"os":           os,
		"arch":         arch,
		"content_hash": result.ContentHash,
		"size_bytes":   result.SizeBytes,
	})
}

func (h *LocalHandler) uploadGeneric(w http.ResponseWriter, r *http.Request, remote *models.Remote, filePath string) {
	existing, err := h.db.GetLocalFile(r.Context(), remote.Name, filePath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if existing != nil {
		http.Error(w, fmt.Sprintf("file %q already exists; overwrites are not allowed", filePath), http.StatusConflict)
		return
	}

	contentType := "application/octet-stream"
	if ct := r.Header.Get("Content-Type"); ct != "" && ct != "application/octet-stream" {
		contentType = ct
	}

	result, err := h.cas.Store(r.Context(), r.Body, contentType)
	if err != nil {
		http.Error(w, fmt.Sprintf("store failed: %v", err), http.StatusInternalServerError)
		return
	}

	if err := h.db.UpsertBlob(r.Context(), result.ContentHash, result.S3Key, result.SizeBytes, contentType); err != nil {
		http.Error(w, fmt.Sprintf("record blob: %v", err), http.StatusInternalServerError)
		return
	}

	if err := h.db.CreateLocalFile(r.Context(), remote.Name, filePath, result.ContentHash); err != nil {
		if errors.Is(err, database.ErrAlreadyExists) {
			http.Error(w, fmt.Sprintf("file %q already exists; overwrites are not allowed", filePath), http.StatusConflict)
			return
		}
		http.Error(w, fmt.Sprintf("record file: %v", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"path":         filePath,
		"content_hash": result.ContentHash,
		"size_bytes":   result.SizeBytes,
	})
}

func (h *LocalHandler) download(w http.ResponseWriter, r *http.Request) {
	repoName := chi.URLParam(r, "name")
	filePath := chi.URLParam(r, "*")

	file, err := h.db.GetLocalFile(r.Context(), repoName, filePath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if file == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	s3Key := storage.BlobKey(file.ContentHash[len("sha256:"):])
	reader, info, err := h.store.Download(r.Context(), s3Key)
	if err != nil {
		http.Error(w, fmt.Sprintf("download failed: %v", err), http.StatusInternalServerError)
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", info.ContentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size))
	w.WriteHeader(http.StatusOK)
	io.Copy(w, reader)
}

func (h *LocalHandler) remove(w http.ResponseWriter, r *http.Request) {
	repoName := chi.URLParam(r, "name")
	filePath := chi.URLParam(r, "*")

	if err := h.db.DeleteLocalFile(r.Context(), repoName, filePath); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type terraformIndex struct {
	Versions map[string]json.RawMessage `json:"versions"`
}

type terraformVersionDoc struct {
	Archives map[string]terraformArchive `json:"archives"`
}

type terraformArchive struct {
	URL    string   `json:"url"`
	Hashes []string `json:"hashes,omitempty"`
}

func (h *LocalHandler) ServeTerraformIndex(w http.ResponseWriter, r *http.Request, repoName, namespace, typeName string) {
	prefix := fmt.Sprintf("%s/%s/", namespace, typeName)
	files, err := h.db.ListLocalFilesByPrefix(r.Context(), repoName, prefix)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	versions := map[string]json.RawMessage{}
	for _, f := range files {
		filename := strings.TrimPrefix(f.FilePath, prefix)
		m := providerZipRe.FindStringSubmatch(filename)
		if m == nil {
			continue
		}
		versions[m[2]] = json.RawMessage(`{}`)
	}

	if len(versions) == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(terraformIndex{Versions: versions})
}

func (h *LocalHandler) ServeTerraformVersionDoc(w http.ResponseWriter, r *http.Request, repoName, namespace, typeName, version string) {
	prefix := fmt.Sprintf("%s/%s/terraform-provider-%s_%s_", namespace, typeName, typeName, version)
	files, err := h.db.ListLocalFilesByPrefix(r.Context(), repoName, prefix)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	archives := map[string]terraformArchive{}
	for _, f := range files {
		filename := strings.TrimPrefix(f.FilePath, fmt.Sprintf("%s/%s/", namespace, typeName))
		m := providerZipRe.FindStringSubmatch(filename)
		if m == nil || m[2] != version {
			continue
		}
		platform := m[3] + "_" + m[4]
		archive := terraformArchive{URL: filename}
		if f.ContentHash != "" {
			archive.Hashes = []string{"zh:" + strings.TrimPrefix(f.ContentHash, "sha256:")}
		}
		archives[platform] = archive
	}

	if len(archives) == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(terraformVersionDoc{Archives: archives})
}
