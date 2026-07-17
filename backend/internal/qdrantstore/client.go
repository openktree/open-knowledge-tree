// Package qdrantstore wraps the Qdrant gRPC client for the
// embedding + dedup pipeline. It is transport-agnostic (no HTTP)
// and the only Qdrant integration point in the application.
//
// Qdrant is a dumb vector index here: point payloads carry
// `{repository_id, status}` only — no fact text, no source_id.
// Postgres (okt_repository.facts + fact_sources) is the single
// source of truth for everything except the vector. This keeps
// Qdrant replaceable and matches the "Qdrant as a search utility"
// framing: a fact is never read from Qdrant except by id (for
// payload update / delete); the vector is only used to find the
// nearest neighbor within a repository.
//
// The collection layout is a single shared collection
// (`okt_facts` by default), payload-filtered by `repository_id`.
// Tier isolation lives in Postgres; Qdrant respects repo
// boundaries via payload filters plus a payload index on
// `repository_id`.
package qdrantstore

import (
	"context"
	"fmt"

	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/qdrant/go-client/qdrant"
)

// Store wraps a Qdrant client and the collection names all point
// operations target. Two collections are used: `collection` for fact
// vectors (the embed_facts → deduplicate_facts pipeline) and
// `conceptCollection` for concept vectors (the embed_concepts worker).
// Both collections share a single gRPC client; the separation keeps
// fact searches from scanning concept vectors and vice versa, and
// lets a dimension change on one collection proceed without forcing
// a re-embedding of the other. Methods on Store are safe for
// concurrent use — the underlying qdrant.Client is a pooled gRPC
// client.
type Store struct {
	client           *qdrant.Client
	collection       string
	conceptCollection string
	// allowRecreate is captured from config so EnsureCollection
	// (the only method that acts on a dimension mismatch) can
	// decide whether to drop+recreate or fail. It is false in
	// production; true is a dev affordance for dimension switches.
	allowRecreate bool
}

// NewClient builds a Store from the Qdrant config. It opens the
// gRPC connection (port 6334 by default) but does NOT create the
// collection — callers (cmd/app/api.go) invoke EnsureCollection
// after the registry is up so the boot log shows a clean
// "collection ready" line. Returning the collection name on the
// Store keeps every point method call-site single-arg (no risk of
// passing the wrong collection string).
func NewClient(cfg config.QdrantConfig) (*Store, error) {
	if cfg.Host == "" {
		// A missing host is a programming error at this point —
		// the wiring layer should not have called NewClient. We
		// return an error rather than defaulting so a misconfigured
		// deploy fails loudly at boot instead of silently talking
		// to localhost.
		return nil, fmt.Errorf("qdrantstore: config host is required")
	}
	port := cfg.Port
	if port == 0 {
		port = 6334
	}
	collection := cfg.Collection
	if collection == "" {
		collection = "okt_facts"
	}
	conceptCollection := cfg.ConceptCollection
	if conceptCollection == "" {
		conceptCollection = "okt_concepts"
	}
	client, err := qdrant.NewClient(&qdrant.Config{
		Host:    cfg.Host,
		Port:    port,
		APIKey:  cfg.APIKey,
		SkipCompatibilityCheck: true,
	})
	if err != nil {
		return nil, fmt.Errorf("qdrantstore: creating client: %w", err)
	}
	return &Store{
		client:            client,
		collection:        collection,
		conceptCollection: conceptCollection,
		allowRecreate:     cfg.AllowRecreate,
	}, nil
}

// Close releases the gRPC connection. Safe to call multiple times.
func (s *Store) Close() error {
	if s == nil || s.client == nil {
		return nil
	}
	return s.client.Close()
}

// HealthCheck pings Qdrant. Used by the wiring layer at boot and
// by tests to decide whether to skip (mirrors the env-gated
// serper/openalex pattern). Returns the server version on success.
func (s *Store) HealthCheck(ctx context.Context) (string, error) {
	if s == nil || s.client == nil {
		return "", fmt.Errorf("qdrantstore: nil store")
	}
	res, err := s.client.HealthCheck(ctx)
	if err != nil {
		return "", err
	}
	return res.GetVersion(), nil
}

// Collection returns the configured facts collection name. Exposed so
// tests (and a future admin health endpoint) can assert which
// collection the server is talking to without reaching into the
// config.
func (s *Store) Collection() string { return s.collection }

// ConceptCollection returns the configured concepts collection name.
// Exposed so tests and the wiring layer can assert which collection
// the server is talking to for concept vectors.
func (s *Store) ConceptCollection() string { return s.conceptCollection }