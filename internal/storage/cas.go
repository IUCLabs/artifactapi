package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

type CAS struct {
	s3 *S3
}

func NewCAS(s3 *S3) *CAS {
	return &CAS{s3: s3}
}

type CASResult struct {
	ContentHash   string
	S3Key         string
	SizeBytes     int64
	AlreadyExists bool
}

// ErrStoreUnavailable marks a Store failure on the object store side (exists check or upload), as
// opposed to a failure reading the source. A pull-through cache can still serve the content uncached.
var ErrStoreUnavailable = errors.New("object store unavailable")

func (c *CAS) Store(ctx context.Context, reader io.Reader, contentType string) (*CASResult, error) {
	tmp, err := os.CreateTemp("", "artifact-*")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	defer func() { _ = tmp.Close() }()

	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, hasher), reader)
	if err != nil {
		return nil, fmt.Errorf("write temp file: %w", err)
	}

	hash := hex.EncodeToString(hasher.Sum(nil))
	s3Key := BlobKey(hash)

	exists, err := c.s3.Exists(ctx, s3Key)
	if err != nil {
		return nil, fmt.Errorf("check blob exists: %w: %w", ErrStoreUnavailable, err)
	}

	if !exists {
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek temp file: %w", err)
		}
		if err := c.s3.Upload(ctx, s3Key, tmp, size, contentType); err != nil {
			return nil, fmt.Errorf("upload blob: %w: %w", ErrStoreUnavailable, err)
		}
	}

	return &CASResult{
		ContentHash:   fmt.Sprintf("sha256:%s", hash),
		S3Key:         s3Key,
		SizeBytes:     size,
		AlreadyExists: exists,
	}, nil
}

func BlobKey(hash string) string {
	return fmt.Sprintf("blobs/sha256/%s", hash)
}

func IndexKey(remote, path string) string {
	return fmt.Sprintf("indexes/%s/%s", remote, path)
}
