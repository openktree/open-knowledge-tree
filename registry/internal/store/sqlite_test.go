package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/openktree/knowledge-registry/internal/model"
)

// TestMigrateSQLite_AddsPromptsetHashToLegacyDB reproduces the dev
// Fly.io crash: a registry volume created by an older binary (whose
// CREATE TABLE decompositions had no promptset_hash column) must be
// upgraded by migrateSQLite so the current INSERT/SELECT statements
// that reference the column stop failing with "no such column".
//
// The dev registry runs SQLite on a persistent /data volume, so the
// table survives across deploys; CREATE TABLE IF NOT EXISTS is a
// no-op on the existing table, so the new column has to be backfilled
// via the ensureColumn ALTER path.
func TestMigrateSQLite_AddsPromptsetHashToLegacyDB(t *testing.T) {
	db, err := sql.Open("sqlite", "file::memory:?cache=shared&_pragma=busy_timeout=5000")
	if err != nil {
		t.Fatalf("opening sqlite: %v", err)
	}
	defer db.Close()
	t.Cleanup(func() { db.Close() })

	// Seed the legacy schema: decompositions WITHOUT promptset_hash,
	// plus the parent sources/repositories tables so the FK and the
	// later INSERT succeed. This is the shape the dev volume carried
	// before the 0.5.6 deploy.
	for _, ddl := range []string{
		`CREATE TABLE repositories (id TEXT PRIMARY KEY, name TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', owner TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE sources (id TEXT PRIMARY KEY, repo_id TEXT NOT NULL, url TEXT, doi TEXT, sha256 TEXT, title TEXT, s3_key TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, UNIQUE(url), UNIQUE(doi))`,
		`CREATE TABLE decompositions (id TEXT PRIMARY KEY, source_id TEXT NOT NULL, model_id TEXT NOT NULL, decomposed_by TEXT NOT NULL DEFAULT '', decomposed_at TEXT, fact_count INTEGER NOT NULL DEFAULT 0, summary_count INTEGER NOT NULL DEFAULT 0, has_embeddings INTEGER NOT NULL DEFAULT 0, embedding_model TEXT, embedding_dims INTEGER NOT NULL DEFAULT 0, s3_key TEXT NOT NULL, created_at TEXT NOT NULL, UNIQUE(source_id, model_id))`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("seeding legacy schema: %v", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO repositories (id,name,description,owner,created_at,updated_at) VALUES ('default','Test','','','2026-01-01','2026-01-01')`); err != nil {
		t.Fatalf("seeding repo: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO sources (id,repo_id,url,doi,sha256,title,s3_key,created_at,updated_at) VALUES ('src-1','default','http://x','','','T','k','2026-01-01','2026-01-01')`); err != nil {
		t.Fatalf("seeding source: %v", err)
	}

	// Run the migration under test. On the legacy DB this must ALTER
	// decompositions to add promptset_hash; on a fresh DB the CREATE
	// TABLE already has it and ensureColumn is a no-op.
	if err := migrateSQLite(db); err != nil {
		t.Fatalf("migrateSQLite on legacy DB: %v", err)
	}

	// The current INSERT (IndexDecomposition) and SELECT
	// (ListDecompositions) reference promptset_hash; both must work
	// against the migrated table. This is the exact path that
	// crashed the dev registry on boot.
	s := &SQLiteStore{db: db}
	meta := &model.DecompMeta{
		ID:            "d1",
		SourceID:      "src-1",
		ModelID:       "m",
		FactCount:     1,
		S3Key:         "k",
		PromptsetHash: "abc123",
		CreatedAt:     time.Now().UTC(),
	}
	if err := s.IndexDecomposition(context.Background(), meta); err != nil {
		t.Fatalf("IndexDecomposition after migrate: %v", err)
	}
	got, err := s.GetDecompositionBySourceAndModel(context.Background(), "src-1", "m")
	if err != nil {
		t.Fatalf("GetDecompositionBySourceAndModel after migrate: %v", err)
	}
	if got.PromptsetHash != "abc123" {
		t.Errorf("promptset_hash after migrate = %q, want abc123", got.PromptsetHash)
	}

	// Re-run migrateSQLite: ensureColumn must be a no-op on a table
	// that already has the column (no "duplicate column" error).
	if err := migrateSQLite(db); err != nil {
		t.Fatalf("migrateSQLite re-run (idempotent): %v", err)
	}
}

// TestMigrateSQLite_FreshDBHasPromptsetHash confirms the fresh-DB
// path still works: the CREATE TABLE includes promptset_hash and
// ensureColumn is a no-op, so IndexDecomposition works immediately.
func TestMigrateSQLite_FreshDBHasPromptsetHash(t *testing.T) {
	s, err := NewSQLiteStore("file::memory:?cache=shared&_pragma=busy_timeout=5000")
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if err := s.CreateRepository(context.Background(), &model.Repository{
		ID: "default", Name: "T", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	if err := s.IndexSource(context.Background(), &model.SourceMeta{
		ID: "src-1", RepoID: "default", S3Key: "k", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("IndexSource: %v", err)
	}
	if err := s.IndexDecomposition(context.Background(), &model.DecompMeta{
		ID: "d1", SourceID: "src-1", ModelID: "m", S3Key: "k", PromptsetHash: "h", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("IndexDecomposition on fresh DB: %v", err)
	}
}
