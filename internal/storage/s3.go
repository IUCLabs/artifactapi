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
}

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

	s := &S3{client: client, bucket: bucket}

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

func (s *S3) Download(ctx context.Context, key string) (io.ReadCloser, *minio.ObjectInfo, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, nil, err
	}

	info, err := obj.Stat()
	if err != nil {
		obj.Close()
		return nil, nil, err
	}

	return obj, &info, nil
}

func (s *S3) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
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
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

func (s *S3) Stat(ctx context.Context, key string) (*minio.ObjectInfo, error) {
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return nil, err
	}
	return &info, nil
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
