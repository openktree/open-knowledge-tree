package storage

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

type LocalStore struct {
	root string
}

func NewLocalStore(root string) (*LocalStore, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	return &LocalStore{root: abs}, nil
}

func (s *LocalStore) key(k string) string {
	return filepath.Join(s.root, filepath.FromSlash(k))
}

func (s *LocalStore) Store(ctx context.Context, key string, body []byte, contentType string) error {
	full := s.key(key)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, body, 0o644)
}

func (s *LocalStore) Get(ctx context.Context, key string) (StoredFile, error) {
	full := s.key(key)
	f, err := os.Open(full)
	if err != nil {
		if os.IsNotExist(err) {
			return StoredFile{}, ErrNotFound
		}
		return StoredFile{}, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return StoredFile{}, err
	}
	return StoredFile{Body: f, Size: info.Size()}, nil
}

func (s *LocalStore) Delete(ctx context.Context, key string) error {
	full := s.key(key)
	if err := os.Remove(full); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

func (s *LocalStore) PresignedURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return "", fmt.Errorf("presigned urls not supported on filesystem backend")
}

func (s *LocalStore) PresignedPUTURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return "", fmt.Errorf("presigned urls not supported on filesystem backend")
}

func (s *LocalStore) ServeURL(key string) string {
	return "/files/" + url.PathEscape(key)
}
