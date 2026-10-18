package provider

// ContentValidator checks an upload's bytes (spooled to localFile) before it is stored.
type ContentValidator interface {
	ValidateContent(storagePath, localFile string) error
}
