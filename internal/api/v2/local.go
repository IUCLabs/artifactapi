package v2

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/storage"
	"github.com/IUCLabs/artifactapi/pkg/models"
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

	prov, _ := provider.Get(remote.PackageType)

	if uploader, ok := prov.(provider.LocalUploader); ok {
		h.uploadValidated(w, r, remote, filePath, prov, uploader)
		return
	}

	h.uploadGeneric(w, r, remote, filePath)
}

func (h *LocalHandler) uploadValidated(w http.ResponseWriter, r *http.Request, remote *models.Remote, filePath string, prov provider.Provider, uploader provider.LocalUploader) {
	storagePath, contentType, err := uploader.ValidateUpload(filePath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	existing, err := h.db.GetLocalFile(r.Context(), remote.Name, storagePath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if existing != nil {
		http.Error(w, fmt.Sprintf("file %q already exists; overwrites are not allowed", storagePath), http.StatusConflict)
		return
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

	if err := h.db.CreateLocalFile(r.Context(), remote.Name, storagePath, result.ContentHash); err != nil {
		if errors.Is(err, database.ErrAlreadyExists) {
			http.Error(w, fmt.Sprintf("file %q already exists; overwrites are not allowed", storagePath), http.StatusConflict)
			return
		}
		http.Error(w, fmt.Sprintf("record file: %v", err), http.StatusInternalServerError)
		return
	}

	if hook, ok := prov.(provider.PostUploadHook); ok {
		go hook.AfterUpload(context.Background(), remote.Name, storagePath, result.ContentHash, h, h.db)
	}

	writeJSON(w, http.StatusCreated, uploader.UploadResponse(storagePath, result.ContentHash, result.SizeBytes))
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

	if err := deleteLocalFile(r.Context(), h.db, repoName, filePath); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteLocalFile removes a local file and runs the provider's post-delete hook,
// so provider-derived state (e.g. RPM metadata that feeds generated repodata)
// stops referencing a package that no longer exists.
func deleteLocalFile(ctx context.Context, db *database.DB, repoName, filePath string) error {
	if err := db.DeleteLocalFile(ctx, repoName, filePath); err != nil {
		return err
	}

	remote, err := db.GetRemote(ctx, repoName)
	if err != nil {
		return nil // file is gone; no repo left to resolve a cleanup hook from
	}
	prov, err := provider.Get(remote.PackageType)
	if err != nil {
		return nil
	}
	if hook, ok := prov.(provider.PostDeleteHook); ok {
		return hook.AfterDelete(ctx, repoName, filePath, db)
	}
	return nil
}

func (h *LocalHandler) DB() *database.DB {
	return h.db
}

func (h *LocalHandler) Download(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	reader, info, err := h.store.Download(ctx, key)
	if err != nil {
		return nil, 0, err
	}
	return reader, info.Size, nil
}
