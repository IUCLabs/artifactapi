package provider

type LocalUploader interface {
	ValidateUpload(filePath string) (storagePath, contentType string, err error)
	UploadResponse(storagePath, contentHash string, sizeBytes int64) map[string]any
}
