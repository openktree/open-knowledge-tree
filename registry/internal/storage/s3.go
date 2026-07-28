package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
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

// StoreStream uploads body to S3 via PutObject with a reader body,
// avoiding the in-memory []byte buffer that Store requires. The AWS
// SDK natively streams from an io.Reader, so multi-GB bundles don't
// buffer in registry memory. Used by the graph push handler.
//
// If body is not seekable (the common case — the Push handler hands
// us an io.MultiReader wrapping a tee buffer + r.Body), the SDK can't
// rewind it for a PutObject retry, so a transient failure on a
// multi-GB upload fails the whole push with "request stream is not
// seekable". To make retries safe, StoreStream spools a non-seekable
// body to a temp file first, then uploads the seekable temp file.
// This trades a second disk write on the registry side for retry
// safety; the OKT export worker already spooled to a temp file on
// its side, so the bytes hit disk twice total instead of once. The
// temp file is removed before StoreStream returns.
func (s *S3Store) StoreStream(ctx context.Context, key string, body io.Reader, contentType string) (int64, error) {
	uploadBody := body
	// Probe for seekability. io.MultiReader (tee buffer + r.Body,
	// what the Push handler builds) is non-seekable, and the AWS SDK
	// can't rewind a non-seekable stream to retry a failed PutObject —
	// a transient failure on a multi-GB upload then fails the whole
	// push with "request stream is not seekable". When the body isn't
	// seekable, spool it to a temp file so PutObject can retry safely.
	if _, seekable := body.(io.ReadSeeker); !seekable {
		f, err := os.CreateTemp("", "okt-registry-s3-*.bin")
		if err != nil {
			return 0, fmt.Errorf("creating s3 spool temp file: %w", err)
		}
		tmpName := f.Name()
		defer func() {
			_ = f.Close()
			_ = os.Remove(tmpName)
		}()
		if _, err := io.Copy(f, body); err != nil {
			return 0, fmt.Errorf("spooling body to temp file: %w", err)
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return 0, fmt.Errorf("rewinding spool temp file: %w", err)
		}
		uploadBody = f
	}

	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        uploadBody,
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return 0, err
	}
	// PutObject doesn't report bytes written; return -1 to signal
	// "unknown" to callers that only care about success.
	return -1, nil
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
