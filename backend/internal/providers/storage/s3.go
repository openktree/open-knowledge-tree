package storage

import (
	"context"
	"errors"

	"github.com/openktree/open-knowledge-tree/backend/internal/config"
)

// S3Storage is the reserved cloud / CDN storage backend. It is NOT
// implemented today; NewS3Storage returns ErrNotImplemented so the
// wiring layer fails loudly at boot if an operator switches
// `providers.storage.backend` to `s3` before the implementation
// lands. The constructor and methods exist so:
//
//   - the storage package's public API is stable for the future
//     implementation (no churn to callers when S3 lands);
//   - the type can be referenced in tests / interfaces / diagrams
//     as the concrete S3 backend;
//   - operators see a clear, named error rather than a generic
//     "unknown backend".
//
// When the S3 / CDN implementation is added, replace the bodies of
// these methods with the real AWS SDK calls (or a CDN client) and
// drop the ErrNotImplemented returns. The interface contract stays
// the same.
type S3Storage struct {
	cfg config.S3Config
}

// NewS3Storage is reserved for the future cloud/CDN provider. It
// always returns ErrNotImplemented today.
func NewS3Storage(cfg config.S3Config) (*S3Storage, error) {
	return nil, ErrNotImplemented
}

func (s *S3Storage) Store(ctx context.Context, key, contentType string, body []byte) (StoredRef, error) {
	return StoredRef{}, errors.Join(ErrNotImplemented, errS3Stub)
}

func (s *S3Storage) Get(ctx context.Context, key string) (StoredFile, error) {
	return StoredFile{}, errors.Join(ErrNotImplemented, errS3Stub)
}

func (s *S3Storage) Delete(ctx context.Context, key string) error {
	return errors.Join(ErrNotImplemented, errS3Stub)
}

func (s *S3Storage) Describe() ProviderDescription {
	return ProviderDescription{
		Name:        "S3 / CDN (reserved)",
		Description: "Cloud object storage backend. Not implemented yet; switch `providers.storage.backend` to `s3` once the implementation lands.",
		Configured:  false,
		Notes:       "Returns ErrNotImplemented at boot.",
	}
}

var errS3Stub = errors.New("storage: s3 backend is a stub")