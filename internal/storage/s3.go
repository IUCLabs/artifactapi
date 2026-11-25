package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type S3 struct {
	client *minio.Client
	bucket string
	health *Health
	// meta, when set, serves metadata calls; RequestPath sets it to fast, which has no client retries.
	meta      *minio.Client
	fast      *minio.Client
	opTimeout time.Duration
}

const (
	RequestTimeout = 2 * time.Second
	ProbeInterval  = 3 * time.Second
	ProbeTimeout   = 1500 * time.Millisecond
)

func NewS3(endpoint, accessKey, secretKey, bucket string, secure bool, region string) (*S3, error) {
	opts := &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: secure,
	}
	if region != "" {
		opts.Region = region
	}

	client, err := minio.New(endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("create s3 client: %w", err)
	}
	fastOpts := *opts
	fastOpts.MaxRetries = 1
	fast, err := minio.New(endpoint, &fastOpts)
	if err != nil {
		return nil, fmt.Errorf("create s3 client: %w", err)
	}

	s := &S3{client: client, bucket: bucket, health: &Health{}, fast: fast}

	if err := s.ensureBucket(context.Background()); err != nil {
		return nil, err
	}

	return s, nil
}

func (s *S3) ensureBucket(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("check bucket: %w", err)
	}
	if !exists {
		if err := s.client.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{}); err != nil {
			return fmt.Errorf("create bucket: %w", err)
		}
		slog.Info("created bucket", "bucket", s.bucket)
	}
	return nil
}

// RequestPath returns a view of s for latency-sensitive callers: metadata calls get one attempt
// under a short deadline, uploads keep the retrying client. It shares s's health state.
func (s *S3) RequestPath() *S3 {
	v := *s
	if v.fast != nil {
		v.meta = v.fast
	}
	v.opTimeout = RequestTimeout
	return &v
}

// Healthy reports the last known state of the object store.
func (s *S3) Healthy() bool { return s.health.Healthy() }

// RunHealthProbe checks the bucket every interval until ctx is done. A passing probe is the only
// thing that marks the store healthy again.
func (s *S3) RunHealthProbe(ctx context.Context, interval, timeout time.Duration) {
	probe := s.fast
	if probe == nil {
		probe = s.client
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		pctx, cancel := context.WithTimeout(ctx, timeout)
		exists, err := probe.BucketExists(pctx, s.bucket)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err == nil && !exists {
			err = fmt.Errorf("bucket %s missing", s.bucket)
		}
		s.health.set(err)
	}
}

// observe marks the store unhealthy on a failure that isn't the caller's cancellation or a
// definitive answer from a reachable store.
func (s *S3) observe(parent context.Context, err error) error {
	if err == nil || parent.Err() != nil {
		return err
	}
	if code := minio.ToErrorResponse(err).StatusCode; code > 0 && code < http.StatusInternalServerError {
		return err
	}
	s.health.set(err)
	return err
}

func (s *S3) metaClient() *minio.Client {
	if s.meta != nil {
		return s.meta
	}
	return s.client
}

func (s *S3) callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.opTimeout == 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, s.opTimeout)
}

// ErrUnhealthy refuses an upload while the store is unhealthy.
var ErrUnhealthy = errors.New("object store unhealthy")

// Upload starts only on a healthy store, then uses the retrying client with no deadline of its own.
// Its outcome never changes the health state.
func (s *S3) Upload(ctx context.Context, key string, reader io.Reader, size int64, contentType string) error {
	if !s.Healthy() {
		return ErrUnhealthy
	}
	_, err := s.client.PutObject(ctx, s.bucket, key, reader, size, minio.PutObjectOptions{
		ContentType: contentType,
	})
	return err
}

// Download bounds only the time to the object's metadata; the body streams without a deadline.
func (s *S3) Download(ctx context.Context, key string) (io.ReadCloser, *minio.ObjectInfo, error) {
	cctx, cancel := context.WithCancel(ctx)
	stop := func() bool { return true }
	if s.opTimeout > 0 {
		stop = time.AfterFunc(s.opTimeout, cancel).Stop
	}
	obj, err := s.metaClient().GetObject(cctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		cancel()
		return nil, nil, s.observe(ctx, err)
	}

	info, err := obj.Stat()
	if err == nil && !stop() {
		err = context.DeadlineExceeded
	}
	if err != nil {
		_ = obj.Close()
		cancel()
		return nil, nil, s.observe(ctx, err)
	}

	return cancelOnClose{obj, cancel}, &info, nil
}

type cancelOnClose struct {
	*minio.Object
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	defer c.cancel()
	return c.Object.Close()
}

func (s *S3) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.Stat(ctx, key)
	if err != nil {
		resp := minio.ToErrorResponse(err)
		if resp.Code == "NoSuchKey" {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	cctx, cancel := s.callCtx(ctx)
	defer cancel()
	return s.observe(ctx, s.metaClient().RemoveObject(cctx, s.bucket, key, minio.RemoveObjectOptions{}))
}

func (s *S3) Stat(ctx context.Context, key string) (*minio.ObjectInfo, error) {
	cctx, cancel := s.callCtx(ctx)
	defer cancel()
	info, err := s.metaClient().StatObject(cctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return nil, s.observe(ctx, err)
	}
	return &info, nil
}

// DeletePrefix removes every object whose key starts with prefix. It lists
// first so a failed list returns its error instead of deleting nothing.
func (s *S3) DeletePrefix(ctx context.Context, prefix string) error {
	var objs []minio.ObjectInfo
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return obj.Err
		}
		objs = append(objs, obj)
	}
	ch := make(chan minio.ObjectInfo, len(objs))
	for _, obj := range objs {
		ch <- obj
	}
	close(ch)
	var err error
	for res := range s.client.RemoveObjects(ctx, s.bucket, ch, minio.RemoveObjectsOptions{}) {
		if res.Err != nil && err == nil {
			err = res.Err
		}
	}
	return err
}

// ListStaleObjects returns keys under prefix last modified before cutoff. Used
// by the GC to reap abandoned staging objects (e.g. cancelled docker pushes).
func (s *S3) ListStaleObjects(ctx context.Context, prefix string, cutoff time.Time) ([]string, error) {
	var keys []string
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		if obj.LastModified.Before(cutoff) {
			keys = append(keys, obj.Key)
		}
	}
	return keys, nil
}
