package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

var ErrPresignDisabled = errors.New("storage: presigning is disabled (no presign_base_url configured)")

type S3Store struct {
	client        *s3.Client
	presignClient *s3.PresignClient // nil when presigning is disabled (dev)
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
	PresignBaseURL string
}

func NewS3Store(cfg S3Config) (*S3Store, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("s3 endpoint is required")
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("s3 bucket is required")
	}

	// Build a custom endpoint resolver that strips the scheme and uses
	// path-style addressing (required for R2).
	endpoint := strings.TrimPrefix(cfg.Endpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "http://")
	endpoint = strings.TrimRight(endpoint, "/")

	usePathStyle := cfg.PathStyle

	creds := credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")

	// Determine the signing region. R2 uses "auto".
	region := cfg.Region
	if region == "" {
		region = "auto"
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(creds),
	)
	if err != nil {
		return nil, fmt.Errorf("loading aws config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String("https://" + endpoint)
		o.UsePathStyle = usePathStyle
	})

	// Ensure the bucket exists (create if not).
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, err = client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(cfg.Bucket)})
	if err != nil {
		// Try to create the bucket. If it already exists, HeadBucket may fail
		// on R2 (returns 400), so treat any error as "maybe create it".
		_, createErr := client.CreateBucket(ctx, &s3.CreateBucketInput{
			Bucket: aws.String(cfg.Bucket),
		})
		if createErr != nil {
			// BucketAlreadyOwnedByYou or BucketAlreadyExists are fine.
			var owned *s3types.BucketAlreadyOwnedByYou
			var exists *s3types.BucketAlreadyExists
			if !errors.As(createErr, &owned) && !errors.As(createErr, &exists) {
				return nil, fmt.Errorf("creating bucket %q: %w", cfg.Bucket, createErr)
			}
		}
	}

	var presignClient *s3.PresignClient
	if cfg.PresignBaseURL != "" {
		presignClient = s3.NewPresignClient(client)
	}

	return &S3Store{client: client, presignClient: presignClient, bucket: cfg.Bucket, region: region}, nil
}

func (s *S3Store) Store(ctx context.Context, key string, body []byte, contentType string) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String(contentType),
	})
	return err
}

func (s *S3Store) Get(ctx context.Context, key string) (StoredFile, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return StoredFile{}, err
	}
	return StoredFile{
		Body:        out.Body,
		ContentType: aws.ToString(out.ContentType),
		Size:        aws.ToInt64(out.ContentLength),
	}, nil
}

func (s *S3Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	return err
}

func (s *S3Store) PresignedURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if s.presignClient == nil {
		return "", ErrPresignDisabled
	}
	signed, err := s.presignClient.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, func(o *s3.PresignOptions) {
		o.Expires = ttl
	})
	if err != nil {
		return "", fmt.Errorf("presigning %s: %w", key, err)
	}
	return signed.URL, nil
}

func (s *S3Store) PresignedPUTURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if s.presignClient == nil {
		return "", ErrPresignDisabled
	}
	signed, err := s.presignClient.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		ContentType: aws.String("application/octet-stream"),
	}, func(o *s3.PresignOptions) {
		o.Expires = ttl
	})
	if err != nil {
		return "", fmt.Errorf("presigning put %s: %w", key, err)
	}
	return signed.URL, nil
}

func (s *S3Store) ServeURL(key string) string {
	return "/files/" + url.PathEscape(key)
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
