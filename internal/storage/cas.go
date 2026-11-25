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

// ErrStoreUnavailable marks a failure on the object store side, as opposed to reading the source.
var ErrStoreUnavailable = errors.New("object store unavailable")

// StoreUnavailableError carries the hashed source when the object store rejects it. Spool reads
// those bytes and removes its temp file on Close.
type StoreUnavailableError struct {
	Err   error
	Spool io.ReadCloser
	Size  int64
}

func (e *StoreUnavailableError) Error() string { return e.Err.Error() }

func (e *StoreUnavailableError) Unwrap() []error { return []error{ErrStoreUnavailable, e.Err} }

type spoolFile struct{ *os.File }

func (f spoolFile) Close() error {
	err := f.File.Close()
	_ = os.Remove(f.Name())
	return err
}

func (c *CAS) Store(ctx context.Context, reader io.Reader, contentType string) (*CASResult, error) {
	res, err := c.StoreOrSpool(ctx, reader, contentType)
	var unavailable *StoreUnavailableError
	if errors.As(err, &unavailable) {
		_ = unavailable.Spool.Close()
	}
	return res, err
}

// StoreOrSpool is Store, except that an object store failure returns a *StoreUnavailableError
// whose Spool the caller must Close.
func (c *CAS) StoreOrSpool(ctx context.Context, reader io.Reader, contentType string) (_ *CASResult, err error) {
	tmp, err := os.CreateTemp("", "artifact-*")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	spool := spoolFile{tmp}
	defer func() {
		var unavailable *StoreUnavailableError
		if !errors.As(err, &unavailable) {
			_ = spool.Close()
		}
	}()

	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, hasher), reader)
	if err != nil {
		return nil, fmt.Errorf("write temp file: %w", err)
	}

	hash := hex.EncodeToString(hasher.Sum(nil))
	s3Key := BlobKey(hash)

	exists, err := c.s3.Exists(ctx, s3Key)
	if err != nil {
		return nil, storeFailure(ctx, "check blob exists", err, spool, size)
	}

	if !exists {
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek temp file: %w", err)
		}
		if err := c.s3.Upload(ctx, s3Key, tmp, size, contentType); err != nil {
			return nil, storeFailure(ctx, "upload blob", err, spool, size)
		}
	}

	return &CASResult{
		ContentHash:   fmt.Sprintf("sha256:%s", hash),
		S3Key:         s3Key,
		SizeBytes:     size,
		AlreadyExists: exists,
	}, nil
}

func storeFailure(ctx context.Context, op string, err error, spool spoolFile, size int64) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w", op, ctx.Err())
	}
	if _, serr := spool.Seek(0, io.SeekStart); serr != nil {
		return fmt.Errorf("%s: %w (seek temp file: %v)", op, err, serr)
	}
	return &StoreUnavailableError{Err: fmt.Errorf("%s: %w", op, err), Spool: spool, Size: size}
}

func BlobKey(hash string) string {
	return fmt.Sprintf("blobs/sha256/%s", hash)
}

func IndexKey(remote, path string) string {
	return fmt.Sprintf("indexes/%s/%s", remote, path)
}
