package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LocalFileStorage persists files to a directory on local disk. It is
// the default and only fully-implemented backend today. Files are
// written atomically (temp file in the same directory + `rename`) so
// a crash mid-write never leaves a partial file visible to Get. The
// ETag is the SHA-256 of the stored bytes, which the serving endpoint
// uses to answer `If-None-Match` 304s.
//
// The root directory is created at construction time. Keys are
// validated to prevent path traversal (`..`, absolute paths, drive
// letters) so a malicious key can't escape the root.
type LocalFileStorage struct {
	root string
}

// NewLocalFileStorage returns a filesystem-backed FileStorage rooted
// at `root` (relative to the process working directory unless
// absolute). The directory and any missing parents are created with
// 0755 perms; files inside are written with 0644.
func NewLocalFileStorage(root string) (*LocalFileStorage, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("storage: filesystem root is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	return &LocalFileStorage{root: abs}, nil
}

// Root returns the absolute path files are stored under. Exposed for
// diagnostics / health checks.
func (s *LocalFileStorage) Root() string { return s.root }

func (s *LocalFileStorage) Store(ctx context.Context, key, contentType string, body []byte) (StoredRef, error) {
	if err := validateKey(key); err != nil {
		return StoredRef{}, err
	}
	full := s.fullPath(key)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return StoredRef{}, err
	}
	sum := sha256.Sum256(body)
	etag := hex.EncodeToString(sum[:])

	// Atomic write: temp file in the same directory, then rename.
	tmp, err := os.CreateTemp(filepath.Dir(full), ".okt-store-tmp-*")
	if err != nil {
		return StoredRef{}, err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := io.Copy(tmp, bytes.NewReader(body)); err != nil {
		_ = tmp.Close()
		cleanup()
		return StoredRef{}, err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return StoredRef{}, err
	}
	if err := os.Rename(tmpName, full); err != nil {
		cleanup()
		return StoredRef{}, err
	}
	if err := os.Chmod(full, 0o644); err != nil {
		return StoredRef{}, err
	}
	return StoredRef{
		Key:         key,
		ContentType: contentType,
		Bytes:       int64(len(body)),
		ETag:        etag,
		StoredAt:    time.Now().UTC(),
	}, nil
}

func (s *LocalFileStorage) Get(ctx context.Context, key string) (StoredFile, error) {
	if err := validateKey(key); err != nil {
		return StoredFile{}, err
	}
	full := s.fullPath(key)
	f, err := os.Open(full)
	if err != nil {
		if os.IsNotExist(err) {
			return StoredFile{}, ErrNotFound
		}
		return StoredFile{}, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return StoredFile{}, err
	}
	// ContentType is not stored as file metadata on disk; callers
	// that need it should record it in the DB (source_images /
	// sources) at Store time and use it when serving. We sniff from
	// the stored bytes' magic so the serving endpoint has a sane
	// default when the DB column is empty.
	ct := sniffContentType(full, bodyPeek(f))
	return StoredFile{
		ContentType: ct,
		Body:        f,
		Size:        info.Size(),
		ModTime:     info.ModTime().UTC(),
	}, nil
}

func (s *LocalFileStorage) Delete(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	full := s.fullPath(key)
	if err := os.Remove(full); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

func (s *LocalFileStorage) Describe() ProviderDescription {
	return ProviderDescription{
		Name:        "Local filesystem",
		Description: "Stores source assets (images, PDFs) on local disk under the configured root.",
		Configured:  true,
		Notes:       "Suitable for single-node deploys; mount a persistent volume in production.",
	}
}

// fullPath joins the validated key onto the root. Caller must have
// already run validateKey.
func (s *LocalFileStorage) fullPath(key string) string {
	return filepath.Join(s.root, filepath.FromSlash(key))
}

// validateKey rejects keys that could escape the storage root.
// Allowed: relative paths using `/` as separator, with no `..`
// components and no absolute/drive-letter prefixes.
func validateKey(key string) error {
	if key == "" {
		return errors.New("storage: empty key")
	}
	if strings.ContainsAny(key, "\x00") {
		return errors.New("storage: key contains NUL")
	}
	cleaned := filepath.Clean(filepath.FromSlash(key))
	if filepath.IsAbs(cleaned) {
		return errors.New("storage: key must not be absolute")
	}
	// Reject any `..` component. filepath.Clean collapses leading
	// `../`, so a leading `..` after Clean is the surest signal.
	if strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) || cleaned == ".." {
		return errors.New("storage: key escapes root via '..'")
	}
	for _, part := range strings.Split(cleaned, string(filepath.Separator)) {
		if part == ".." {
			return errors.New("storage: key escapes root via '..'")
		}
	}
	// Windows drive-letter guard (e.g. "C:foo"); harmless on Linux.
	if len(cleaned) >= 2 && cleaned[1] == ':' && ('a' <= cleaned[0] && cleaned[0] <= 'z' || 'A' <= cleaned[0] && cleaned[0] <= 'Z') {
		return errors.New("storage: key must not start with a drive letter")
	}
	return nil
}

// bodyPeek reads a small head of the file for content-type sniffing.
// It seeks the file back to offset 0 before returning so the caller's
// Read sees the whole file. The reader is only used transiently.
func bodyPeek(f *os.File) []byte {
	const peek = 512
	buf := make([]byte, peek)
	n, _ := io.ReadFull(f, buf[:])
	_, _ = f.Seek(0, io.SeekStart)
	if n < 0 {
		return nil
	}
	return buf[:n]
}

// sniffContentType uses net/http's sniff on the first 512 bytes. The
// `path` is used for extension fallback (e.g. `.pdf`) when sniffing
// is inconclusive.
func sniffContentType(path string, head []byte) string {
	if ct := http.DetectContentType(head); ct != "application/octet-stream" {
		return ct
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".pdf":
		return "application/pdf"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	}
	return "application/octet-stream"
}