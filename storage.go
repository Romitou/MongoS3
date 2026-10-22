package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// backupObjectPrefix is shared by every object created by this tool, it is
// what retention relies on to never touch unrelated objects of the bucket.
const backupObjectPrefix = "dump."

type storage struct {
	client *minio.Client
	cfg    *config
}

func newStorage(cfg *config) (*storage, error) {
	client, err := minio.New(cfg.S3Endpoint, &minio.Options{
		Creds:        newCredentials(cfg),
		Secure:       cfg.S3Secure,
		Region:       cfg.S3Region,
		BucketLookup: bucketLookup(cfg.S3URLStyle),
	})
	if err != nil {
		return nil, fmt.Errorf("create S3 client: %w", err)
	}
	return &storage{client: client, cfg: cfg}, nil
}

func newCredentials(cfg *config) *credentials.Credentials {
	if cfg.S3ID != "" {
		return credentials.NewStaticV4(cfg.S3ID, cfg.S3Key, cfg.S3SessionToken)
	}
	return credentials.NewChainCredentials([]credentials.Provider{
		&credentials.EnvAWS{},
		&credentials.EnvMinio{},
		&credentials.FileAWSCredentials{},
		&credentials.IAM{},
	})
}

func bucketLookup(style string) minio.BucketLookupType {
	switch style {
	case "path":
		return minio.BucketLookupPath
	case "virtual":
		return minio.BucketLookupDNS
	default:
		return minio.BucketLookupAuto
	}
}

func (s *storage) checkBucket(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.cfg.S3Bucket)
	if err != nil {
		return fmt.Errorf("check bucket %q: %w", s.cfg.S3Bucket, err)
	}
	if !exists {
		return fmt.Errorf("bucket %q does not exist", s.cfg.S3Bucket)
	}
	return nil
}

// uploadStream uploads a reader of unknown size with a multipart upload.
// The upload is aborted if reading fails, so a failed dump never ends up as a
// truncated archive.
func (s *storage) uploadStream(ctx context.Context, key string, reader io.Reader) (minio.UploadInfo, error) {
	info, err := s.client.PutObject(ctx, s.cfg.S3Bucket, key, reader, -1, minio.PutObjectOptions{
		ContentType: "application/octet-stream",
		PartSize:    s.cfg.PartSize,
	})
	if err != nil {
		return info, fmt.Errorf("upload %q: %w", key, err)
	}
	return info, nil
}

func (s *storage) uploadText(ctx context.Context, key, text string) (minio.UploadInfo, error) {
	reader := strings.NewReader(text)
	info, err := s.client.PutObject(ctx, s.cfg.S3Bucket, key, reader, reader.Size(), minio.PutObjectOptions{ContentType: "text/plain; charset=utf-8"})
	if err != nil {
		return info, fmt.Errorf("upload %q: %w", key, err)
	}
	return info, nil
}

func (s *storage) presign(ctx context.Context, key string) (*url.URL, error) {
	u, err := s.client.PresignedGetObject(ctx, s.cfg.S3Bucket, key, s.cfg.PresignExpiry, nil)
	if err != nil {
		return nil, fmt.Errorf("presign %q: %w", key, err)
	}
	return u, nil
}

// listBackups lists every object created by this tool.
func (s *storage) listBackups(ctx context.Context) ([]minio.ObjectInfo, error) {
	var objects []minio.ObjectInfo
	for object := range s.client.ListObjects(ctx, s.cfg.S3Bucket, minio.ListObjectsOptions{Prefix: s.cfg.S3Prefix + backupObjectPrefix}) {
		if object.Err != nil {
			return nil, fmt.Errorf("list backups: %w", object.Err)
		}
		objects = append(objects, object)
	}
	return objects, nil
}

func (s *storage) remove(ctx context.Context, key string) error {
	if err := s.client.RemoveObject(ctx, s.cfg.S3Bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("delete %q: %w", key, err)
	}
	return nil
}
