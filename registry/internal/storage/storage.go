package storage

import (
	"context"
	"errors"
	"io"
	"time"
)

var ErrNotFound = errors.New("storage: object not found")

type StoredFile struct {
	Body        io.ReadCloser
	ContentType string
	Size        int64
}

type FileStore interface {
	Store(ctx context.Context, key string, body []byte, contentType string) error
	// StoreStream writes body to the backend without buffering the
	// whole object in a []byte. Used by the graph push handler for
	// multi-GB bundles that would otherwise exceed the registry's
	// memory budget. Returns the number of bytes written.
	StoreStream(ctx context.Context, key string, body io.Reader, contentType string) (int64, error)
	Get(ctx context.Context, key string) (StoredFile, error)
	Delete(ctx context.Context, key string) error
	PresignedURL(ctx context.Context, key string, ttl time.Duration) (string, error)
	PresignedPUTURL(ctx context.Context, key string, ttl time.Duration) (string, error)
}
