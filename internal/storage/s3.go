package storage

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type S3 struct {
	client *minio.Client
	bucket string
	// fast has no client retries; RequestPath routes metadata calls through it under metaTimeout.
	fast        *minio.Client
	metaTimeout time.Duration
}

const RequestTimeout = 2 * time.Second

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

	s := &S3{client: client, bucket: bucket, fast: fast}

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

func (s *S3) Upload(ctx context.Context, key string, reader io.Reader, size int64, contentType string) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, reader, size, minio.PutObjectOptions{
		ContentType: contentType,
	})
	return err
}

// RequestPath returns a view of s whose Exists, Stat and Download head make one attempt under
// RequestTimeout; uploads keep the retrying client.
func (s *S3) RequestPath() *S3 {
	v := *s
	v.metaTimeout = RequestTimeout
	return &v
}

// Retrying returns a view of s whose Download head uses the retrying client with no deadline.
func (s *S3) Retrying() *S3 {
	v := *s
	v.metaTimeout = 0
	return &v
}

func (s *S3) metaClient() *minio.Client {
	if s.metaTimeout > 0 {
		return s.fast
	}
	return s.client
}

// Probe checks that the bucket is reachable, with a single attempt.
func (s *S3) Probe(ctx context.Context) error {
	exists, err := s.fast.BucketExists(ctx, s.bucket)
	if err == nil && !exists {
		err = fmt.Errorf("bucket %s missing", s.bucket)
	}
	return err
}

// Download bounds only the time to the object's metadata; the body streams without a deadline.
func (s *S3) Download(ctx context.Context, key string) (io.ReadCloser, *minio.ObjectInfo, error) {
	cctx, cancel := context.WithCancel(ctx)
	stop := func() bool { return true }
	if s.metaTimeout > 0 {
		stop = time.AfterFunc(s.metaTimeout, cancel).Stop
	}
	obj, err := s.metaClient().GetObject(cctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		cancel()
		return nil, nil, err
	}

	info, err := obj.Stat()
	if !stop() && ctx.Err() == nil {
		err = context.DeadlineExceeded
	}
	if err != nil {
		_ = obj.Close()
		cancel()
		return nil, nil, err
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
		if IsNoSuchKey(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func IsNoSuchKey(err error) bool {
	return minio.ToErrorResponse(err).Code == "NoSuchKey"
}

func (s *S3) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

func (s *S3) Stat(ctx context.Context, key string) (*minio.ObjectInfo, error) {
	if s.metaTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.metaTimeout)
		defer cancel()
	}
	info, err := s.metaClient().StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return nil, err
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
