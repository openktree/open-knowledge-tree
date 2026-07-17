// Command backfill-embedding-models is a one-time maintenance CLI
// that rewrites the embedding model identifier stored on every
// registry decomposition to the bare model name (stripping the
// provider routing prefix and OpenRouter variant tags), so the
// backend's registry cache reconciler recognizes cross-provider
// equivalence on pull and doesn't trigger a needless full re-embed.
//
// The model string lives in TWO places per decomposition:
//   1. the decompositions.embedding_model DB column (indexed metadata)
//   2. the DecompositionPackage JSON blob in S3/R2 (the
//      Embeddings.Model / ConceptEmbeddings.Model fields)
//
// This tool rewrites both, using model.NormalizeEmbeddingModel (a
// mirror of backend/internal/providers/ai.NormalizeEmbeddingModel).
//
// Usage:
//
//	# Dry run (default) — logs what would change, writes nothing.
//	backfill-embedding-models <config.yaml>
//
//	# Apply the rewrite.
//	backfill-embedding-models -apply <config.yaml>
//
// The tool is idempotent: re-running on already-normalized data is a
// no-op. Paginates through all decompositions via
// ListAllDecompositions (500/page).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openktree/knowledge-registry/internal/config"
	"github.com/openktree/knowledge-registry/internal/model"
	"github.com/openktree/knowledge-registry/internal/storage"
	"github.com/openktree/knowledge-registry/internal/store"
)

func main() {
	apply := flag.Bool("apply", false, "apply the rewrite (default is dry-run)")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: backfill-embedding-models [-apply] <config.yaml>")
		os.Exit(2)
	}
	cfgPath := flag.Arg(0)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	ctx := context.Background()

	mstore, err := openMetadataStore(ctx, cfg)
	if err != nil {
		log.Fatalf("opening metadata store: %v", err)
	}
	defer mstore.Close()

	s3store, err := openStorage(cfg)
	if err != nil {
		log.Fatalf("opening storage: %v", err)
	}

	mode := "DRY RUN"
	if *apply {
		mode = "APPLY"
	}
	log.Printf("backfill-embedding-models [%s]: driver=%s storage=%s bucket=%s", mode, cfg.Database.Driver, cfg.Storage.Backend, cfg.S3.Bucket)

	stats := runBackfill(ctx, mstore, s3store, *apply)

	log.Printf("backfill-embedding-models [%s] done: scanned=%d, model_changed=%d, s3_rewritten=%d, db_updated=%d, skipped=%d",
		mode, stats.scanned, stats.modelChanged, stats.s3Rewritten, stats.dbUpdated, stats.skipped)
	if !*apply {
		log.Printf("backfill-embedding-models: dry run only — re-run with -apply to persist changes")
	}
}

type backfillStats struct {
	scanned     int
	modelChanged int
	s3Rewritten int
	dbUpdated   int
	skipped     int
}

func runBackfill(ctx context.Context, mstore store.MetadataStore, s3 *storage.S3Store, apply bool) backfillStats {
	var stats backfillStats
	const pageSize = 500
	for offset := 0; ; offset += pageSize {
		decomps, err := mstore.ListAllDecompositions(ctx, pageSize, offset)
		if err != nil {
			log.Printf("backfill: listing decompositions at offset %d: %v", offset, err)
			break
		}
		if len(decomps) == 0 {
			break
		}
		for _, d := range decomps {
			stats.scanned++
			processDecomp(ctx, mstore, s3, d, apply, &stats)
		}
		if len(decomps) < pageSize {
			break
		}
	}
	return stats
}

func processDecomp(ctx context.Context, mstore store.MetadataStore, s3 *storage.S3Store, d model.DecompMeta, apply bool, stats *backfillStats) {
	origModel := d.EmbeddingModel
	if origModel == "" {
		// No embedding model recorded — nothing to normalize.
		stats.skipped++
		return
	}
	normalized := model.NormalizeEmbeddingModel(origModel)
	if normalized == origModel {
		// Already bare — no change needed.
		stats.skipped++
		return
	}
	stats.modelChanged++
	log.Printf("  decomp %s (source=%s model=%s): %q → %q", d.ID, d.SourceID, d.ModelID, origModel, normalized)

	if !apply {
		return
	}

	// 1. Rewrite the S3 blob's Embeddings.Model / ConceptEmbeddings.Model.
	if d.S3Key != "" && s3 != nil {
		if err := rewriteS3Blob(ctx, s3, d.S3Key, normalized); err != nil {
			log.Printf("    S3 rewrite %s FAILED: %v", d.S3Key, err)
		} else {
			stats.s3Rewritten++
		}
	}

	// 2. Update the DB column.
	if err := mstore.UpdateDecompositionEmbeddingModel(ctx, d.ID, normalized); err != nil {
		log.Printf("    DB update %s FAILED: %v", d.ID, err)
	} else {
		stats.dbUpdated++
	}
}

// rewriteS3Blob reads the decomposition JSON, rewrites the
// Embeddings.Model + ConceptEmbeddings.Model fields to the normalized
// value, and stores the blob back to the same key.
func rewriteS3Blob(ctx context.Context, s3 *storage.S3Store, s3Key, normalizedModel string) error {
	data, _, err := s3.ReadAll(ctx, s3Key)
	if err != nil {
		return fmt.Errorf("reading blob: %w", err)
	}
	var decomp model.DecompositionPackage
	if err := json.Unmarshal(data, &decomp); err != nil {
		return fmt.Errorf("decoding blob: %w", err)
	}
	changed := false
	if decomp.Embeddings != nil && decomp.Embeddings.Model != normalizedModel {
		decomp.Embeddings.Model = normalizedModel
		changed = true
	}
	if decomp.ConceptEmbeddings != nil && decomp.ConceptEmbeddings.Model != normalizedModel {
		decomp.ConceptEmbeddings.Model = normalizedModel
		changed = true
	}
	if !changed {
		// The DB column said the model differed but the blob
		// either has no embeddings or is already normalized. Log
		// and skip the write to avoid a needless S3 PUT.
		return nil
	}
	body, err := json.Marshal(decomp)
	if err != nil {
		return fmt.Errorf("encoding blob: %w", err)
	}
	if err := s3.StoreJSON(ctx, s3Key, body); err != nil {
		return fmt.Errorf("writing blob: %w", err)
	}
	return nil
}

func openMetadataStore(ctx context.Context, cfg *config.Config) (store.MetadataStore, error) {
	switch cfg.Database.Driver {
	case "sqlite", "":
		return store.NewSQLiteStore(cfg.Database.URL)
	case "postgres":
		pool, err := pgxpool.New(ctx, cfg.Database.URL)
		if err != nil {
			return nil, fmt.Errorf("connecting to postgres: %w", err)
		}
		return store.NewPostgresStore(pool), nil
	default:
		return nil, fmt.Errorf("unknown database driver: %s", cfg.Database.Driver)
	}
}

func openStorage(cfg *config.Config) (*storage.S3Store, error) {
	if cfg.Storage.Backend != "s3" && cfg.Storage.Backend != "" {
		return nil, fmt.Errorf("unsupported storage backend for backfill: %s (only s3 supported)", cfg.Storage.Backend)
	}
	s3store, err := storage.NewS3Store(storage.S3Config{
		Endpoint:       cfg.S3.Endpoint,
		Region:         cfg.S3.Region,
		Bucket:         cfg.S3.Bucket,
		AccessKey:      cfg.S3.AccessKey,
		SecretKey:      cfg.S3.SecretKey,
		PathStyle:      cfg.S3.PathStyle,
		PresignTTL:     cfg.S3.PresignTTL,
		PresignBaseURL: cfg.S3.PresignBaseURL,
	})
	if err != nil {
		return nil, fmt.Errorf("setting up s3 storage: %w", err)
	}
	return s3store, nil
}