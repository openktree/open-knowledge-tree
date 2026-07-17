package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ErrPresignDisabled is returned by PresignedURL / PresignedPUTURL
// when no presign client was configured (PresignBaseURL was empty).
// Callers should treat this as "skip presigning" rather than a hard
// error.
var ErrPresignDisabled = errors.New("storage: presigning is disabled (no presign_base_url configured)")

type S3Store struct {
	client        *minio.Client
	presignClient *minio.Client // nil when presigning is disabled (dev)
	bucket        string
	region        string
}

type S3Config struct {
	Endpoint       string
	Region         string
	Bucket         string
	AccessKey      string
	SecretKey      string
	PathStyle      bool
	PresignTTL     int
	PresignBaseURL string // public-facing endpoint for presigned URLs; empty = don't presign
}

func NewS3Store(cfg S3Config) (*S3Store, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("s3 endpoint is required")
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("s3 bucket is required")
	}

	// Parse the endpoint: accept both bare hostnames and full URLs.
	// minio-go expects a bare "host:port" string, not "https://host".
	secure := false
	endpoint := cfg.Endpoint
	if strings.HasPrefix(endpoint, "https://") {
		secure = true
		endpoint = strings.TrimPrefix(endpoint, "https://")
	} else if strings.HasPrefix(endpoint, "http://") {
		endpoint = strings.TrimPrefix(endpoint, "http://")
	}
	endpoint = strings.TrimRight(endpoint, "/")

	lookup := minio.BucketLookupAuto
	if cfg.PathStyle {
		lookup = minio.BucketLookupPath
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       secure,
		Region:       cfg.Region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("creating s3 client: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("checking bucket %q: %w", cfg.Bucket, err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{Region: cfg.Region}); err != nil {
			return nil, fmt.Errorf("creating bucket %q: %w", cfg.Bucket, err)
		}
	}

	// Build a separate presign client when a public-facing base
	// URL is configured. This client points at the external
	// endpoint (e.g. https://s3.example.com) so the presigned
	// URLs it generates are reachable by browsers. The internal
	// client above keeps using the Docker-network endpoint
	// (e.g. minio:9000) for server-to-server operations.
	// When PresignBaseURL is empty, presignClient stays nil and
	// PresignedURL / PresignedPUTURL return ErrPresignDisabled —
	// callers should skip embedding presigned URLs in that case.
	var presignClient *minio.Client
	if cfg.PresignBaseURL != "" {
		presignSecure := strings.HasPrefix(cfg.PresignBaseURL, "https")
		presignEndpoint := cfg.PresignBaseURL
		if strings.HasPrefix(presignEndpoint, "https://") {
			presignEndpoint = strings.TrimPrefix(presignEndpoint, "https://")
		} else if strings.HasPrefix(presignEndpoint, "http://") {
			presignEndpoint = strings.TrimPrefix(presignEndpoint, "http://")
		}
		presignEndpoint = strings.TrimRight(presignEndpoint, "/")
		presignClient, err = minio.New(presignEndpoint, &minio.Options{
			Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
			Secure:       presignSecure,
			Region:       cfg.Region,
			BucketLookup: lookup,
		})
		if err != nil {
			return nil, fmt.Errorf("creating presign s3 client: %w", err)
		}
	}

	return &S3Store{client: client, presignClient: presignClient, bucket: cfg.Bucket, region: cfg.Region}, nil
}

func (s *S3Store) key(k string) string {
	return k
}

func (s *S3Store) Store(ctx context.Context, key string, body []byte, contentType string) error {
	_, err := s.client.PutObject(ctx, s.bucket, s.key(key),
		bytes.NewReader(body), int64(len(body)),
		minio.PutObjectOptions{ContentType: contentType})
	return err
}

func (s *S3Store) Get(ctx context.Context, key string) (StoredFile, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, s.key(key), minio.GetObjectOptions{})
	if err != nil {
		return StoredFile{}, err
	}
	stat, err := obj.Stat()
	if err != nil {
		obj.Close()
		return StoredFile{}, fmt.Errorf("stat %s: %w", key, err)
	}
	return StoredFile{
		Body:        obj,
		ContentType: stat.ContentType,
		Size:        stat.Size,
	}, nil
}

func (s *S3Store) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, s.key(key), minio.RemoveObjectOptions{})
}

func (s *S3Store) PresignedURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if s.presignClient == nil {
		return "", ErrPresignDisabled
	}
	u, err := s.presignClient.PresignedGetObject(ctx, s.bucket, s.key(key), ttl, nil)
	if err != nil {
		return "", fmt.Errorf("presigning %s: %w", key, err)
	}
	return u.String(), nil
}

func (s *S3Store) PresignedPUTURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if s.presignClient == nil {
		return "", ErrPresignDisabled
	}
	u, err := s.presignClient.PresignedPutObject(ctx, s.bucket, s.key(key), ttl)
	if err != nil {
		return "", fmt.Errorf("presigning put %s: %w", key, err)
	}
	return u.String(), nil
}

func (s *S3Store) ReadAll(ctx context.Context, key string) ([]byte, string, error) {
	f, err := s.Get(ctx, key)
	if err != nil {
		return nil, "", err
	}
	defer f.Body.Close()
	data, err := io.ReadAll(io.LimitReader(f.Body, 100<<20))
	if err != nil {
		return nil, "", err
	}
	return data, f.ContentType, nil
}

func (s *S3Store) StoreJSON(ctx context.Context, key string, data []byte) error {
	return s.Store(ctx, key, data, "application/json")
}
