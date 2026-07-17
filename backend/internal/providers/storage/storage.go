// Package storage contains the file-storage provider interface plus
// concrete backend implementations. The interface is the contract the
// retrieve_source worker and the HTTP serving endpoint depend on; it
// is transport-agnostic (no HTTP, no DB) so it can be reused by a
// CLI, a future CDN sync worker, etc.
//
// Files are keyed by an opaque string chosen by the caller. The
// canonical key scheme is
// `repositories/{repoID}/sources/{sourceID}/images/{imageID}.{ext}`
// for images and `repositories/{repoID}/sources/{sourceID}/body.pdf`
// for full PDF source bodies. Backends MUST treat the key as opaque
// (do not parse it for semantics) but MAY use its path-like shape to
// lay out files on disk (the filesystem backend maps `key → <root>/<key>`).
//
// The backend is pluggable so the project can swap local-disk storage
// for cloud / CDN storage without touching callers. Today only
// `LocalFileStorage` (filesystem) is implemented; an `S3Storage` stub
// marks the extension point for the future cloud provider.
package storage

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/internal/config"
)

// ErrNotImplemented is returned by a backend that is reserved but
// not yet wired up (today: the S3 backend). Callers should treat it
// as a fatal configuration error at boot rather than a runtime error.
var ErrNotImplemented = errors.New("storage: backend not implemented")

// ErrNotFound is returned by Get/Delete when no file exists at the
// given key. Callers map this to HTTP 404.
var ErrNotFound = errors.New("storage: object not found")

// FileStorage is the pluggable file-storage backend. Implementations
// must be safe for concurrent use.
type FileStorage interface {
	// Store writes `body` to the backend under `key`, recording
	// `contentType` as the MIME to serve back on Get. It MUST be
	// idempotent in the sense that storing the same key twice
	// succeeds and leaves the latest bytes visible. Callers
	// should pass a non-empty contentType so the serving
	// endpoint can set Content-Type without re-sniffing.
	Store(ctx context.Context, key, contentType string, body []byte) (StoredRef, error)

	// Get opens the file at `key` for streaming. The caller MUST
	// close `StoredFile.Body`. Returns ErrNotFound when the key
	// has no file.
	Get(ctx context.Context, key string) (StoredFile, error)

	// Delete removes the file at `key`. Returns ErrNotFound when
	// the key has no file. Idempotent deletes of an already-missing
	// key are safe (callers may ignore ErrNotFound during cleanup).
	Delete(ctx context.Context, key string) error

	// Describe returns static metadata for the `/sources/providers`
	// style UI. Backends with nothing useful to advertise can return
	// a zero-value description.
	Describe() ProviderDescription
}

// StoredRef is the result of a successful Store. `Key` is the key the
// caller passed in (echoed back so callers can log/compare); the
// other fields are the backend's record of what it stored.
type StoredRef struct {
	Key         string
	ContentType string
	Bytes       int64
	ETag        string // opaque, typically a hex SHA-256
	StoredAt    time.Time
}

// StoredFile is the result of a successful Get. The caller owns
// `Body` and must close it.
type StoredFile struct {
	ContentType string
	Body        io.ReadCloser
	ETag        string
	Size        int64
	ModTime     time.Time
}

// ProviderDescription mirrors fetch.ProviderDescription so the
// storage backend can be listed in the providers UI the same way
// resolution / search providers are. Fields are intentionally narrow.
type ProviderDescription struct {
	Name        string
	Description string
	Requires    string
	Configured  bool
	Notes       string
}

// NewFromConfig picks the backend named in `cfg.Backend` and returns
// the constructed FileStorage. The default (`filesystem`) is always
// available; `s3` and any unknown backend return ErrNotImplemented
// (the wiring layer treats that as a fatal boot error). Mirrors the
// `NewXxxFromConfig` pattern used by the search/resolution/AI
// providers.
func NewFromConfig(cfg config.StorageConfig) (FileStorage, error) {
	switch cfg.Backend {
	case "filesystem":
		return NewLocalFileStorage(cfg.Filesystem.Root)
	case "s3":
		return nil, ErrNotImplemented
	default:
		return nil, ErrNotImplemented
	}
}