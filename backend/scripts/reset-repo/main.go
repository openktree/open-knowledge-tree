// Command reset-repo wipes all per-repo data (sources, facts,
// concepts, candidates, summaries, syntheses, skips) from the dev
// Postgres + Qdrant for a single repository, leaving the repository
// row itself (and its settings) intact. Intended for the concept-
// regeneration runbook: after wiping, a pull-all (at facts level)
// repopulates sources + facts and the concept pipeline rebuilds
// from a clean slate.
//
// This is a dev operator tool, not a production reset. It connects
// to the dev Postgres (port 5432) and the dev Qdrant (port 6334)
// using the same env vars the API uses. It refuses to run against
// the e2e test DB (port 5433) — use the e2e harness for that.
//
// Usage:
//
//	go run ./scripts/reset-repo <repo-id-or-slug>
//
// The repo identifier is resolved by UUID first, then by slug.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: reset-repo <repo-id-or-slug>")
		os.Exit(2)
	}
	ident := os.Args[1]

	ctx := context.Background()

	dbURL := os.Getenv("OKT_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://okt:okt_dev@localhost:5432/okt?sslmode=disable"
	}
	if err := guardTestDB(dbURL); err != nil {
		fmt.Fprintln(os.Stderr, "refusing to run:", err)
		os.Exit(1)
	}

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("connecting to postgres: %v", err)
	}
	defer pool.Close()

	repoID, err := resolveRepo(ctx, pool, ident)
	if err != nil {
		log.Fatalf("resolving repo %q: %v", ident, err)
	}
	log.Printf("reset-repo: wiping data for repository %s", repoID)

	// Capture fact + concept UUIDs before deleting so we can delete
	// the Qdrant vectors by the same UUIDs.
	factIDs, conceptIDs, err := captureIDs(ctx, pool, repoID)
	if err != nil {
		log.Fatalf("capturing ids: %v", err)
	}
	log.Printf("reset-repo: captured %d facts, %d concepts", len(factIDs), len(conceptIDs))

	// Delete Qdrant vectors first (best-effort — if Qdrant is down
	// we still wipe Postgres, the orphan vectors are harmless).
	qHost := os.Getenv("QDRANT_HOST")
	if qHost == "" {
		qHost = "localhost"
	}
	qPort, _ := strconv.Atoi(os.Getenv("QDRANT_PORT"))
	if qPort == 0 {
		qPort = 6334
	}
	qs, qerr := qdrantstore.NewClient(config.QdrantConfig{Host: qHost, Port: qPort})
	if qerr != nil {
		log.Printf("reset-repo: connecting to qdrant (skipping vector cleanup): %v", qerr)
	} else {
		if err := qs.DeleteFactVectors(ctx, factIDs); err != nil {
			log.Printf("reset-repo: deleting fact vectors (non-fatal): %v", err)
		}
		if err := qs.DeleteConceptVectors(ctx, conceptIDs); err != nil {
			log.Printf("reset-repo: deleting concept vectors (non-fatal): %v", err)
		}
	}

	// Scoped SQL wipe in FK-safe order. Nearly everything cascades
	// from sources/concepts/facts, but concept_syntheses has no FK
	// to concepts.id (keyed by lower(canonical_name)) so it must be
	// deleted explicitly. concept_candidates cascades fact_candidates.
	if err := wipeRepo(ctx, pool, repoID); err != nil {
		log.Fatalf("wiping repo data: %v", err)
	}

	log.Printf("reset-repo: done. Repository %s now has no sources/facts/concepts.", repoID)
	log.Printf("reset-repo: next steps — set pull_level=facts, then POST .../settings/pull-all")
}

func guardTestDB(dbURL string) error {
	// Refuse the e2e test DB (port 5433) — the e2e harness owns that.
	if hasPort(dbURL, "5433") {
		return fmt.Errorf("targeting the e2e test DB (port 5433) — use the dev DB (port 5432)")
	}
	return nil
}

func hasPort(dbURL, port string) bool {
	// crude check: look for ":<port>/" in the URL.
	for i := 0; i+1 < len(dbURL); i++ {
		if dbURL[i] == ':' && i+len(port)+1 <= len(dbURL) {
			if dbURL[i+1:i+1+len(port)] == port {
				return true
			}
		}
	}
	return false
}

func resolveRepo(ctx context.Context, pool *pgxpool.Pool, ident string) (uuid.UUID, error) {
	// Try UUID first.
	if id, err := uuid.Parse(ident); err == nil {
		var exists bool
		err = pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM okt_system.repositories WHERE id = $1)", id).Scan(&exists)
		if err != nil {
			return uuid.Nil, err
		}
		if !exists {
			return uuid.Nil, fmt.Errorf("no repository with id %s", id)
		}
		return id, nil
	}
	// Fall back to slug.
	var id uuid.UUID
	err := pool.QueryRow(ctx, "SELECT id FROM okt_system.repositories WHERE slug = $1", ident).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("no repository with slug %q: %w", ident, err)
	}
	return id, nil
}

func captureIDs(ctx context.Context, pool *pgxpool.Pool, repoID uuid.UUID) (factIDs, conceptIDs []uuid.UUID, err error) {
	// Facts linked to this repo's sources.
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT f.id
		FROM okt_repository.facts f
		JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
		JOIN okt_repository.sources s ON s.id = fs.source_id
		WHERE s.repository_id = $1`, repoID)
	if err != nil {
		return nil, nil, fmt.Errorf("querying fact ids: %w", err)
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, nil, err
		}
		factIDs = append(factIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	// Concepts for this repo.
	rows, err = pool.Query(ctx, `
		SELECT id FROM okt_repository.concepts WHERE repository_id = $1`, repoID)
	if err != nil {
		return nil, nil, fmt.Errorf("querying concept ids: %w", err)
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, nil, err
		}
		conceptIDs = append(conceptIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return factIDs, conceptIDs, nil
}

func wipeRepo(ctx context.Context, pool *pgxpool.Pool, repoID uuid.UUID) error {
	// FK-safe order:
	// 1. concept_syntheses — no FK to concepts.id, keyed by name.
	// 2. concept_candidates — cascades fact_candidates.
	// 3. sources — cascades source_images, fact_sources,
	//    fact_references, investigation_sources.
	// 4. concepts — cascades concept_aliases, fact_concepts,
	//    concept_summaries. concept_candidates.resolved_concept_id
	//    is SET NULL but those rows are already gone from step 2.
	// 5. facts — orphaned after fact_sources cascade. Delete facts
	//    that are no longer referenced by any fact_sources row.
	// 6. fact_concept_skips — cascades from facts, but facts above
	//    only deletes orphans not linked to any source; the skips
	//    for this repo's facts are cleaned by the fact delete.
	//    To be safe, delete skips for the captured fact IDs.
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	exec := func(sql string, args ...any) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	}

	if err := exec(`DELETE FROM okt_repository.concept_syntheses WHERE repository_id = $1`, repoID); err != nil {
		return fmt.Errorf("deleting concept_syntheses: %w", err)
	}
	if err := exec(`DELETE FROM okt_repository.concept_candidates WHERE repository_id = $1`, repoID); err != nil {
		return fmt.Errorf("deleting concept_candidates: %w", err)
	}
	if err := exec(`DELETE FROM okt_repository.sources WHERE repository_id = $1`, repoID); err != nil {
		return fmt.Errorf("deleting sources: %w", err)
	}
	if err := exec(`DELETE FROM okt_repository.concepts WHERE repository_id = $1`, repoID); err != nil {
		return fmt.Errorf("deleting concepts: %w", err)
	}
	// Delete orphan facts (no longer linked to any source). This is
	// repo-scoped because we only captured this repo's fact IDs, but
	// a fact could be shared across repos — only delete if it has no
	// remaining fact_sources links at all.
	if err := exec(`
		DELETE FROM okt_repository.facts f
		WHERE NOT EXISTS (SELECT 1 FROM okt_repository.fact_sources fs WHERE fs.fact_id = f.id)`); err != nil {
		return fmt.Errorf("deleting orphan facts: %w", err)
	}
	// fact_concept_skips cascade from facts, but to be explicit:
	if err := exec(`
		DELETE FROM okt_repository.fact_concept_skips sk
		WHERE NOT EXISTS (SELECT 1 FROM okt_repository.facts f WHERE f.id = sk.fact_id)`); err != nil {
		return fmt.Errorf("deleting orphan fact_concept_skips: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}