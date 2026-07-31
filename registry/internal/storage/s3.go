package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
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

// StoreStream uploads body to S3 using a multipart upload, avoiding
// the in-memory []byte buffer that Store requires. Used by the graph
// push handler for multi-GB bundles.
//
// Multipart upload (CreateMultipartUpload + UploadPart +
// CompleteMultipartUpload) is the standard S3 pattern for large
// objects: body is read in bounded parts (defaultPartSize, 64 MB),
// each part is uploaded with a fresh bytes.Reader (seekable, so the
// SDK can retry the part on a transient failure without rewinding the
// whole body), and the parts are committed at the end. Peak memory
// is one part at a time (64 MB), bounded independently of bundle
// size. On an unrecoverable part failure, the upload is aborted
// (AbortMultipartUpload) so orphaned parts don't accumulate in R2.
//
// This replaces the previous PutObject path, which passed the raw
// non-seekable body and failed on retry with "request stream is not
// seekable"; and the spool-to-temp-file patch, which doubled disk
// write cost. Multipart keeps the body a pure stream (no disk spool)
// while making retries safe per-part.
func (s *S3Store) StoreStream(ctx context.Context, key string, body io.Reader, contentType string) (int64, error) {
	const defaultPartSize = 64 << 20 // 64 MB — well above S3's 5 MB part minimum

	// Initiate the multipart upload.
	createOut, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return 0, fmt.Errorf("creating multipart upload: %w", err)
	}
	uploadID := aws.ToString(createOut.UploadId)
	// Abort on any failure path so orphaned parts don't accrue
	// storage charges in R2. The deferred flag is cleared on the
	// happy path after CompleteMultipartUpload succeeds.
	//
	// The abort uses a fresh context.Background() with a short
	// timeout, NOT the request ctx: a failure often coincides with
	// the request being cancelled (client disconnect, deadline),
	// and an abort against a cancelled ctx silently no-ops — leaving
	// the incomplete multipart upload in the bucket. The cleanup
	// endpoint would eventually reap it, but a deterministic abort
	// here is cheaper and avoids surprise R2 charges.
	aborted := false
	defer func() {
		if !aborted {
			return
		}
		abortCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = s.client.AbortMultipartUpload(abortCtx, &s3.AbortMultipartUploadInput{
			Bucket:   aws.String(s.bucket),
			Key:      aws.String(key),
			UploadId: aws.String(uploadID),
		})
	}()

	// Upload parts. Each part is read into a bounded buffer, wrapped
	// in a bytes.Reader (seekable — the SDK retries the part by
	// rewinding the reader, no body-wide rewind needed).
	var completedParts []s3types.CompletedPart
	partBuf := make([]byte, defaultPartSize)
	var totalBytes int64
	partNumber := int32(1)
	for {
		n, readErr := io.ReadFull(body, partBuf)
		if n > 0 {
			part := partBuf[:n]
			uploadOut, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
				Bucket:     aws.String(s.bucket),
				Key:        aws.String(key),
				UploadId:   aws.String(uploadID),
				PartNumber: aws.Int32(partNumber),
				Body:       bytes.NewReader(part),
			})
			if err != nil {
				aborted = true
				return totalBytes, fmt.Errorf("uploading part %d: %w", partNumber, err)
			}
			completedParts = append(completedParts, s3types.CompletedPart{
				ETag:       uploadOut.ETag,
				PartNumber: aws.Int32(partNumber),
			})
			totalBytes += int64(n)
			partNumber++
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			// io.ReadFull returns ErrUnexpectedEOF when it read
			// some bytes but couldn't fill the buffer — the n>0
			// branch above already uploaded them. EOF means no
			// bytes were read and the body is exhausted.
			break
		}
		if readErr != nil {
			aborted = true
			return totalBytes, fmt.Errorf("reading body part %d: %w", partNumber, readErr)
		}
	}

	// S3 requires at least one part. An empty body (zero-byte upload)
	// is handled by uploading a single empty part.
	if len(completedParts) == 0 {
		uploadOut, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:     aws.String(s.bucket),
			Key:        aws.String(key),
			UploadId:   aws.String(uploadID),
			PartNumber: aws.Int32(1),
			Body:       bytes.NewReader(nil),
		})
		if err != nil {
			aborted = true
			return 0, fmt.Errorf("uploading empty part: %w", err)
		}
		completedParts = append(completedParts, s3types.CompletedPart{
			ETag:       uploadOut.ETag,
			PartNumber: aws.Int32(1),
		})
	}

	// Commit.
	_, err = s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
		MultipartUpload: &s3types.CompletedMultipartUpload{
			Parts: completedParts,
		},
	})
	if err != nil {
		aborted = true
		return totalBytes, fmt.Errorf("completing multipart upload: %w", err)
	}
	return totalBytes, nil
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

// MultipartUploadInfo describes one in-progress (or just-aborted)
// multipart upload surfaced by CleanupMultipartUploads. The fields
// mirror the relevant subset of types.MultipartUpload so the admin
// endpoint's JSON response stays decoupled from the AWS SDK.
type MultipartUploadInfo struct {
	Key       string    `json:"key"`
	UploadID  string    `json:"upload_id"`
	Initiated time.Time `json:"initiated"`
}

// CleanupMultipartUploads lists in-progress multipart uploads in the
// bucket and aborts the ones initiated before `now - maxAge`. A push
// that's still running is younger than maxAge and is left alone; a push
// that died mid-upload leaves an orphan older than maxAge, and its
// parts are released back to R2 (no further storage charges).
//
// `listed` is every upload the bucket reported (orphaned + in-flight);
// `aborted` is the subset that was successfully aborted; `failed`
// carries the orphans whose AbortMultipartUpload call returned an
// error (logged and skipped, not retried here — the next cleanup run
// picks them up). In-flight uploads younger than maxAge are not
// included in `aborted` or `failed`; they're counted in `listed`.
func (s *S3Store) CleanupMultipartUploads(ctx context.Context, maxAge time.Duration) (listed, aborted int, failed []MultipartUploadInfo, err error) {
	cutoff := time.Now().Add(-maxAge)

	var keyMarker, uploadIDMarker *string
	for {
		out, lerr := s.client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
			Bucket:         aws.String(s.bucket),
			KeyMarker:      keyMarker,
			UploadIdMarker: uploadIDMarker,
		})
		if lerr != nil {
			return listed, aborted, failed, fmt.Errorf("listing multipart uploads: %w", lerr)
		}

		for _, u := range out.Uploads {
			listed++
			// S3 may omit Key/UploadId on malformed entries; skip them.
			key := aws.ToString(u.Key)
			uploadID := aws.ToString(u.UploadId)
			if key == "" || uploadID == "" {
				continue
			}
			// nil Initiated (rare) can't be age-checked; treat as orphan
			// so a corrupt entry gets reaped rather than leaking.
			initiated := time.Time{}
			if u.Initiated != nil {
				initiated = *u.Initiated
			}
			if !initiated.Before(cutoff) && !initiated.IsZero() {
				continue
			}
			info := MultipartUploadInfo{
				Key:       key,
				UploadID:  uploadID,
				Initiated: initiated,
			}
			if _, aerr := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
				Bucket:   aws.String(s.bucket),
				Key:      aws.String(key),
				UploadId: aws.String(uploadID),
			}); aerr != nil {
				failed = append(failed, info)
				log.Printf("storage: aborting multipart upload %s/%s: %v", key, uploadID, aerr)
				continue
			}
			aborted++
		}

		// Pagination: S3 returns up to 1000 per call; keep paging
		// until IsTruncated is false (no NextKeyMarker / NextUploadIDMarker).
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		keyMarker = out.NextKeyMarker
		uploadIDMarker = out.NextUploadIdMarker
		if keyMarker == nil && uploadIDMarker == nil {
			break
		}
	}
	return listed, aborted, failed, nil
}
