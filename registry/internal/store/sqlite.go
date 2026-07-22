package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/openktree/knowledge-registry/internal/model"
)

type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(dsn string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	// WAL mode + NORMAL synchronous: avoids a full fsync per commit
	// (DELETE journal mode, the default, fsyncs on every transaction).
	// With the batch fact-hash tx, this turns N fsyncs per push into one.
	// WAL is safe with MaxOpenConns(1) — no concurrent writers to
	// coordinate.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("setting WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA synchronous=NORMAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("setting synchronous=NORMAL: %w", err)
	}
	if err := migrateSQLite(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating sqlite: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

func migrateSQLite(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS repositories (
			id          TEXT PRIMARY KEY,
			name        TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			owner       TEXT NOT NULL DEFAULT '',
			created_at  TEXT NOT NULL,
			updated_at  TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS sources (
			id          TEXT PRIMARY KEY,
			repo_id     TEXT NOT NULL REFERENCES repositories(id),
			url         TEXT,
			doi         TEXT,
			sha256      TEXT,
			title       TEXT,
			s3_key      TEXT NOT NULL,
			created_at  TEXT NOT NULL,
			updated_at  TEXT NOT NULL,
			UNIQUE(url),
			UNIQUE(doi)
		);

		CREATE INDEX IF NOT EXISTS idx_sources_repo ON sources(repo_id);
		CREATE INDEX IF NOT EXISTS idx_sources_sha256 ON sources(sha256);

		CREATE TABLE IF NOT EXISTS decompositions (
			id              TEXT PRIMARY KEY,
			source_id       TEXT NOT NULL REFERENCES sources(id),
			model_id        TEXT NOT NULL,
			decomposed_by   TEXT NOT NULL DEFAULT '',
			decomposed_at   TEXT,
			fact_count      INTEGER NOT NULL DEFAULT 0,
			summary_count   INTEGER NOT NULL DEFAULT 0,
			has_embeddings  INTEGER NOT NULL DEFAULT 0,
			embedding_model TEXT,
			embedding_dims  INTEGER NOT NULL DEFAULT 0,
			s3_key          TEXT NOT NULL,
			created_at      TEXT NOT NULL,
			UNIQUE(source_id, model_id)
		);

		CREATE INDEX IF NOT EXISTS idx_decompositions_source ON decompositions(source_id);
		CREATE INDEX IF NOT EXISTS idx_decompositions_model ON decompositions(model_id);

		CREATE TABLE IF NOT EXISTS fact_hashes (
			content_hash     TEXT NOT NULL,
			source_id        TEXT NOT NULL,
			decomposition_id TEXT NOT NULL,
			fact_id          TEXT NOT NULL,
			created_at       TEXT NOT NULL,
			PRIMARY KEY (content_hash, source_id, decomposition_id)
		);

		CREATE INDEX IF NOT EXISTS idx_fact_hashes_source ON fact_hashes(source_id, content_hash);

		CREATE TABLE IF NOT EXISTS contexts (
			label       TEXT PRIMARY KEY,
			description TEXT NOT NULL DEFAULT '',
			created_at  TEXT NOT NULL,
			updated_at  TEXT NOT NULL
		);

		CREATE INDEX IF NOT EXISTS idx_contexts_label ON contexts(label);

		CREATE TABLE IF NOT EXISTS users (
			id            TEXT PRIMARY KEY,
			email         TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			display_name  TEXT NOT NULL DEFAULT '',
			role          TEXT NOT NULL DEFAULT 'viewer',
			created_at    TEXT NOT NULL,
			updated_at    TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS api_tokens (
			id          TEXT PRIMARY KEY,
			user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name        TEXT NOT NULL,
			token_hash  TEXT NOT NULL UNIQUE,
			scope       TEXT NOT NULL DEFAULT 'read',
			expires_at  TEXT,
			created_at  TEXT NOT NULL
		);

		CREATE INDEX IF NOT EXISTS idx_api_tokens_user ON api_tokens(user_id);

		CREATE TABLE IF NOT EXISTS graphs (
			id             TEXT PRIMARY KEY,
			name           TEXT NOT NULL,
			description    TEXT NOT NULL DEFAULT '',
			owner          TEXT NOT NULL DEFAULT '',
			tags           TEXT NOT NULL DEFAULT '[]',
			source_count   INTEGER NOT NULL DEFAULT 0,
			fact_count     INTEGER NOT NULL DEFAULT 0,
			concept_count  INTEGER NOT NULL DEFAULT 0,
			s3_key         TEXT NOT NULL,
			sha256         TEXT NOT NULL DEFAULT '',
			schema_version INTEGER NOT NULL DEFAULT 1,
			created_at     TEXT NOT NULL,
			updated_at     TEXT NOT NULL
		);

		CREATE INDEX IF NOT EXISTS idx_graphs_name ON graphs(name);
		CREATE INDEX IF NOT EXISTS idx_graphs_created_at ON graphs(created_at DESC);
	`)
	return err
}

func (s *SQLiteStore) Close() error { return s.db.Close() }

func (s *SQLiteStore) CreateRepository(ctx context.Context, repo *model.Repository) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO repositories (id, name, description, owner, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		repo.ID, repo.Name, repo.Description, repo.Owner, repo.CreatedAt.UTC().Format(time.RFC3339), repo.UpdatedAt.UTC().Format(time.RFC3339))
	return err
}

func (s *SQLiteStore) GetRepository(ctx context.Context, id string) (*model.Repository, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, description, owner, created_at, updated_at FROM repositories WHERE id = ?`, id)
	return scanRepo(row)
}

func (s *SQLiteStore) ListRepositories(ctx context.Context) ([]model.Repository, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, description, owner, created_at, updated_at FROM repositories ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Repository
	for rows.Next() {
		r, err := scanRepo(scanRow{rows: rows})
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) UpdateRepository(ctx context.Context, id string, upd model.RepoUpdate) error {
	if upd.Name != nil {
		if _, err := s.db.ExecContext(ctx, `UPDATE repositories SET name = ?, updated_at = ? WHERE id = ?`,
			*upd.Name, time.Now().UTC().Format(time.RFC3339), id); err != nil {
			return err
		}
	}
	if upd.Description != nil {
		if _, err := s.db.ExecContext(ctx, `UPDATE repositories SET description = ?, updated_at = ? WHERE id = ?`,
			*upd.Description, time.Now().UTC().Format(time.RFC3339), id); err != nil {
			return err
		}
	}
	return nil
}

func (s *SQLiteStore) DeleteRepository(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM repositories WHERE id = ?`, id)
	return err
}

func (s *SQLiteStore) IndexSource(ctx context.Context, meta *model.SourceMeta) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sources (id, repo_id, url, doi, sha256, title, s3_key, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   url=excluded.url, doi=excluded.doi, sha256=excluded.sha256,
		   title=excluded.title, s3_key=excluded.s3_key, updated_at=excluded.updated_at`,
		meta.ID, meta.RepoID, nullString(meta.URL), nullString(meta.DOI),
		nullString(meta.SHA256), nullString(meta.Title), meta.S3Key,
		meta.CreatedAt.UTC().Format(time.RFC3339), meta.UpdatedAt.UTC().Format(time.RFC3339))
	return err
}

func (s *SQLiteStore) GetSource(ctx context.Context, repoID, sourceID string) (*model.SourceMeta, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, repo_id, url, doi, sha256, title, s3_key, created_at, updated_at
		 FROM sources WHERE id = ? AND repo_id = ?`, sourceID, repoID)
	return scanSource(row)
}

func (s *SQLiteStore) ListAllSources(ctx context.Context, limit, offset int) ([]model.SourceMeta, error) {
	if limit <= 0 || limit > 500 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, repo_id, url, doi, sha256, title, s3_key, created_at, updated_at
		 FROM sources ORDER BY created_at DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.SourceMeta
	for rows.Next() {
		m, err := scanSource(scanRow{rows: rows})
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) ListSources(ctx context.Context, repoID string, limit, offset int) ([]model.SourceMeta, error) {
	if limit <= 0 || limit > 500 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, repo_id, url, doi, sha256, title, s3_key, created_at, updated_at
		 FROM sources WHERE repo_id = ? ORDER BY created_at DESC LIMIT ? OFFSET ?`, repoID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.SourceMeta
	for rows.Next() {
		m, err := scanSource(scanRow{rows: rows})
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) SearchByURL(ctx context.Context, repoID, url string) ([]model.SourceMeta, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, repo_id, url, doi, sha256, title, s3_key, created_at, updated_at
		 FROM sources WHERE repo_id = ? AND url = ?`, repoID, url)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSources(rows)
}

func (s *SQLiteStore) SearchByDOI(ctx context.Context, repoID, doi string) ([]model.SourceMeta, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, repo_id, url, doi, sha256, title, s3_key, created_at, updated_at
		 FROM sources WHERE repo_id = ? AND doi = ?`, repoID, doi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSources(rows)
}

func (s *SQLiteStore) SearchBySHA256(ctx context.Context, repoID, sha256 string) ([]model.SourceMeta, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, repo_id, url, doi, sha256, title, s3_key, created_at, updated_at
		 FROM sources WHERE repo_id = ? AND sha256 = ?`, repoID, sha256)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSources(rows)
}

func (s *SQLiteStore) IndexDecomposition(ctx context.Context, meta *model.DecompMeta) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO decompositions (id, source_id, model_id, decomposed_by, decomposed_at,
		   fact_count, summary_count, has_embeddings, embedding_model, embedding_dims, s3_key, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   fact_count=excluded.fact_count, summary_count=excluded.summary_count,
		   has_embeddings=excluded.has_embeddings, s3_key=excluded.s3_key`,
		meta.ID, meta.SourceID, meta.ModelID, meta.DecomposedBy,
		nullTime(meta.DecomposedAt), meta.FactCount, meta.SummaryCount,
		btoi(meta.HasEmbeddings), nullString(meta.EmbeddingModel), meta.EmbeddingDims,
		meta.S3Key, meta.CreatedAt.UTC().Format(time.RFC3339))
	return err
}

func (s *SQLiteStore) ListDecompositions(ctx context.Context, sourceID string) ([]model.DecompMeta, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, source_id, model_id, decomposed_by, decomposed_at,
		   fact_count, summary_count, has_embeddings, embedding_model, embedding_dims, s3_key, created_at
		 FROM decompositions WHERE source_id = ? ORDER BY decomposed_at DESC`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDecomps(rows)
}

func (s *SQLiteStore) GetDecompositionBySourceAndModel(ctx context.Context, sourceID, modelID string) (*model.DecompMeta, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, source_id, model_id, decomposed_by, decomposed_at,
		   fact_count, summary_count, has_embeddings, embedding_model, embedding_dims, s3_key, created_at
		 FROM decompositions WHERE source_id = ? AND model_id = ?`, sourceID, modelID)
	return scanDecomp(row)
}

func (s *SQLiteStore) ListAllDecompositions(ctx context.Context, limit, offset int) ([]model.DecompMeta, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, source_id, model_id, decomposed_by, decomposed_at,
		   fact_count, summary_count, has_embeddings, embedding_model, embedding_dims, s3_key, created_at
		 FROM decompositions ORDER BY created_at ASC LIMIT ? OFFSET ?`,
		limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDecomps(rows)
}

func (s *SQLiteStore) UpdateDecompositionEmbeddingModel(ctx context.Context, id, embeddingModel string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE decompositions SET embedding_model = ? WHERE id = ?`,
		embeddingModel, id)
	return err
}

func (s *SQLiteStore) FactHashExists(ctx context.Context, sourceID, hash string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM fact_hashes WHERE content_hash = ? AND source_id = ?`, hash, sourceID).Scan(&n)
	return n > 0, err
}

func (s *SQLiteStore) InsertFactHash(ctx context.Context, hash, sourceID, decompID, factID string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO fact_hashes (content_hash, source_id, decomposition_id, fact_id, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		hash, sourceID, decompID, factID, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *SQLiteStore) LinkFactHash(ctx context.Context, hash, sourceID, decompID string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO fact_hashes (content_hash, source_id, decomposition_id, fact_id, created_at)
		 VALUES (?, ?, ?, '', ?)`,
		hash, sourceID, decompID, time.Now().UTC().Format(time.RFC3339))
	return err
}

// BatchUpsertFactHashes inserts new fact hashes and re-links existing
// ones in a single transaction. Replaces the per-fact loop of
// FactHashExists + InsertFactHash/LinkFactHash (2N auto-committed
// queries) with one tx: one SELECT IN (...) + N INSERTs, all inside
// one WAL fsync. For 200 facts this cuts ~400 queries × ~5ms each
// (~2s) down to one tx (~10ms).
func (s *SQLiteStore) BatchUpsertFactHashes(ctx context.Context, sourceID, decompID string, entries []model.FactHashEntry) (model.BatchFactHashResult, error) {
	if len(entries) == 0 {
		return model.BatchFactHashResult{}, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.BatchFactHashResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Batch SELECT: which hashes already exist for this source?
	// Build (?, ?, ...) placeholder list.
	ph := make([]string, len(entries))
	args := make([]any, 0, len(entries)+1)
	args = append(args, sourceID)
	for i, e := range entries {
		ph[i] = "?"
		args = append(args, e.ContentHash)
	}
	query := `SELECT content_hash FROM fact_hashes WHERE source_id = ? AND content_hash IN (` + strings.Join(ph, ",") + `)`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return model.BatchFactHashResult{}, fmt.Errorf("batch checking fact hashes: %w", err)
	}
	existingSet := make(map[string]bool, len(entries))
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return model.BatchFactHashResult{}, fmt.Errorf("scanning fact hash: %w", err)
		}
		existingSet[h] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return model.BatchFactHashResult{}, fmt.Errorf("fact hash rows: %w", err)
	}

	// Bulk insert new + link existing, inside the tx.
	// Duplicates within the same batch (same content_hash appearing
	// twice in entries) are handled by adding each inserted hash to
	// existingSet as we go, so the second occurrence takes the link
	// path instead of colliding on the PK. This mirrors the original
	// per-fact loop where each FactHashExists saw the previous insert.
	now := time.Now().UTC().Format(time.RFC3339)
	var newCount, linkedCount int
	for _, e := range entries {
		if existingSet[e.ContentHash] {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO fact_hashes (content_hash, source_id, decomposition_id, fact_id, created_at)
				 VALUES (?, ?, ?, '', ?)`,
				e.ContentHash, sourceID, decompID, now); err != nil {
				return model.BatchFactHashResult{}, fmt.Errorf("linking fact hash %s: %w", e.ContentHash, err)
			}
			linkedCount++
		} else {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO fact_hashes (content_hash, source_id, decomposition_id, fact_id, created_at)
				 VALUES (?, ?, ?, ?, ?)`,
				e.ContentHash, sourceID, decompID, e.FactID, now); err != nil {
				return model.BatchFactHashResult{}, fmt.Errorf("inserting fact hash %s: %w", e.ContentHash, err)
			}
			existingSet[e.ContentHash] = true
			newCount++
		}
	}

	if err := tx.Commit(); err != nil {
		return model.BatchFactHashResult{}, fmt.Errorf("commit batch fact hashes: %w", err)
	}
	return model.BatchFactHashResult{New: newCount, Linked: linkedCount}, nil
}

func (s *SQLiteStore) SearchByText(ctx context.Context, repoID, query string, limit, offset int) ([]model.SourceMeta, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	pattern := "%" + query + "%"
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, repo_id, url, doi, sha256, title, s3_key, created_at, updated_at
		 FROM sources WHERE repo_id = ? AND (title LIKE ? OR url LIKE ? OR doi LIKE ?)
		 ORDER BY created_at DESC LIMIT ? OFFSET ?`,
		repoID, pattern, pattern, pattern, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSources(rows)
}

func (s *SQLiteStore) CountByText(ctx context.Context, repoID, query string) (int, error) {
	pattern := "%" + query + "%"
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sources WHERE repo_id = ? AND (title LIKE ? OR url LIKE ? OR doi LIKE ?)`,
		repoID, pattern, pattern, pattern).Scan(&n)
	return n, err
}

func (s *SQLiteStore) CountAllSources(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sources`).Scan(&n)
	return n, err
}

func (s *SQLiteStore) Stats(ctx context.Context) (repoCount, sourceCount int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM repositories`).Scan(&repoCount)
	if err != nil {
		return 0, 0, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sources`).Scan(&sourceCount)
	return
}

// ── Graphs ──────────────────────────────────────────────────────────
//
// SQLite has no array type, so tags is stored as a JSON-encoded string
// (encoding/json) and parsed on read. The Postgres store uses TEXT[];
// the model layer always hands the store a []string, and the SQLite
// layer marshals/unmarshals at the boundary.

func (s *SQLiteStore) IndexGraph(ctx context.Context, meta *model.GraphMeta) error {
	tagsJSON, err := json.Marshal(meta.Tags)
	if err != nil {
		return fmt.Errorf("marshaling graph tags: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO graphs (id, name, description, owner, tags, source_count, fact_count, concept_count, s3_key, sha256, schema_version, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   name=excluded.name, description=excluded.description, owner=excluded.owner,
		   tags=excluded.tags, source_count=excluded.source_count, fact_count=excluded.fact_count,
		   concept_count=excluded.concept_count, s3_key=excluded.s3_key, sha256=excluded.sha256,
		   schema_version=excluded.schema_version, updated_at=excluded.updated_at`,
		meta.ID, meta.Name, meta.Description, meta.Owner, string(tagsJSON),
		meta.SourceCount, meta.FactCount, meta.ConceptCount, meta.S3Key, meta.SHA256,
		meta.SchemaVersion, meta.CreatedAt.UTC().Format(time.RFC3339), meta.UpdatedAt.UTC().Format(time.RFC3339))
	return err
}

func (s *SQLiteStore) GetGraph(ctx context.Context, id string) (*model.GraphMeta, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, description, owner, tags, source_count, fact_count, concept_count, s3_key, sha256, schema_version, created_at, updated_at
		 FROM graphs WHERE id = ?`, id)
	return scanGraph(row)
}

func (s *SQLiteStore) ListGraphs(ctx context.Context, limit, offset int) ([]model.GraphMeta, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, description, owner, tags, source_count, fact_count, concept_count, s3_key, sha256, schema_version, created_at, updated_at
		 FROM graphs ORDER BY created_at DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGraphs(rows)
}

func (s *SQLiteStore) SearchGraphsByText(ctx context.Context, query string, limit, offset int) ([]model.GraphMeta, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	pattern := "%" + query + "%"
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, description, owner, tags, source_count, fact_count, concept_count, s3_key, sha256, schema_version, created_at, updated_at
		 FROM graphs WHERE name LIKE ? OR description LIKE ?
		 ORDER BY created_at DESC LIMIT ? OFFSET ?`,
		pattern, pattern, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGraphs(rows)
}

func (s *SQLiteStore) SearchGraphsByTag(ctx context.Context, tag string, limit, offset int) ([]model.GraphMeta, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	// SQLite: match the tag as a substring of the JSON tags array.
	// This is a LIKE over the JSON string — not index-accelerated, but
	// the graphs table is small (shared graphs are few per registry).
	pattern := `%"` + tag + `"%`
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, description, owner, tags, source_count, fact_count, concept_count, s3_key, sha256, schema_version, created_at, updated_at
		 FROM graphs WHERE tags LIKE ?
		 ORDER BY created_at DESC LIMIT ? OFFSET ?`,
		pattern, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGraphs(rows)
}

func (s *SQLiteStore) CountGraphs(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM graphs`).Scan(&n)
	return n, err
}

func (s *SQLiteStore) CountGraphsByText(ctx context.Context, query string) (int, error) {
	pattern := "%" + query + "%"
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM graphs WHERE name LIKE ? OR description LIKE ?`,
		pattern, pattern).Scan(&n)
	return n, err
}

func (s *SQLiteStore) CountGraphsByTag(ctx context.Context, tag string) (int, error) {
	pattern := `%"` + tag + `"%`
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM graphs WHERE tags LIKE ?`, pattern).Scan(&n)
	return n, err
}

func (s *SQLiteStore) DeleteGraph(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM graphs WHERE id = ?`, id)
	return err
}

func scanGraph(row interface{ Scan(...any) error }) (*model.GraphMeta, error) {
	var id, name, desc, owner, s3key, sha, tagsJSON, ca, ua string
	var srcCount, factCount, conceptCount, schemaVersion int
	if err := row.Scan(&id, &name, &desc, &owner, &tagsJSON, &srcCount, &factCount, &conceptCount, &s3key, &sha, &schemaVersion, &ca, &ua); err != nil {
		return nil, err
	}
	var tags []string
	if tagsJSON != "" {
		_ = json.Unmarshal([]byte(tagsJSON), &tags)
	}
	if tags == nil {
		tags = []string{}
	}
	createdAt, _ := time.Parse(time.RFC3339, ca)
	updatedAt, _ := time.Parse(time.RFC3339, ua)
	return &model.GraphMeta{
		ID: id, Name: name, Description: desc, Owner: owner, Tags: tags,
		SourceCount: srcCount, FactCount: factCount, ConceptCount: conceptCount,
		S3Key: s3key, SHA256: sha, SchemaVersion: schemaVersion,
		CreatedAt: createdAt, UpdatedAt: updatedAt,
	}, nil
}

func scanGraphs(rows interface {
	Next() bool
	Scan(...any) error
	Close() error
	Err() error
}) ([]model.GraphMeta, error) {
	var out []model.GraphMeta
	for rows.Next() {
		m, err := scanGraph(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) UpsertContext(ctx context.Context, label, description string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO contexts (label, description, created_at, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(label) DO UPDATE SET
		   description=excluded.description, updated_at=excluded.updated_at`,
		label, description, now, now)
	return err
}

func (s *SQLiteStore) ListContexts(ctx context.Context) ([]model.ContextClass, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT label, description FROM contexts ORDER BY label`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.ContextClass
	for rows.Next() {
		var c model.ContextClass
		if err := rows.Scan(&c.Label, &c.Description); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type scanRow struct {
	rows interface {
		Scan(dest ...any) error
	}
}

func (s scanRow) Scan(dest ...any) error { return s.rows.Scan(dest...) }

func scanRepo(row interface{ Scan(...any) error }) (*model.Repository, error) {
	var id, name, desc, owner, ca, ua string
	if err := row.Scan(&id, &name, &desc, &owner, &ca, &ua); err != nil {
		return nil, err
	}
	createdAt, _ := time.Parse(time.RFC3339, ca)
	updatedAt, _ := time.Parse(time.RFC3339, ua)
	return &model.Repository{
		ID: id, Name: name, Description: desc, Owner: owner,
		CreatedAt: createdAt, UpdatedAt: updatedAt,
	}, nil
}

func scanSource(row interface{ Scan(...any) error }) (*model.SourceMeta, error) {
	var id, repoID, s3key, ca, ua string
	var url, doi, sha256, title sql.NullString
	if err := row.Scan(&id, &repoID, &url, &doi, &sha256, &title, &s3key, &ca, &ua); err != nil {
		return nil, err
	}
	createdAt, _ := time.Parse(time.RFC3339, ca)
	updatedAt, _ := time.Parse(time.RFC3339, ua)
	return &model.SourceMeta{
		ID: id, RepoID: repoID, URL: url.String, DOI: doi.String,
		SHA256: sha256.String, Title: title.String, S3Key: s3key,
		CreatedAt: createdAt, UpdatedAt: updatedAt,
	}, nil
}

func scanSources(rows interface {
	Next() bool
	Scan(...any) error
	Close() error
	Err() error
}) ([]model.SourceMeta, error) {
	var out []model.SourceMeta
	for rows.Next() {
		m, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func scanDecomp(row interface{ Scan(...any) error }) (*model.DecompMeta, error) {
	var id, sourceID, modelID, s3key, ca string
	var decBy, embModel sql.NullString
	var decAt sql.NullString
	var factCount, summaryCount, embDims, embModelVal int
	if err := row.Scan(&id, &sourceID, &modelID, &decBy, &decAt,
		&factCount, &summaryCount, &embModelVal, &embModel, &embDims, &s3key, &ca); err != nil {
		return nil, err
	}
	createdAt, _ := time.Parse(time.RFC3339, ca)
	var decomposedAt time.Time
	if decAt.Valid {
		decomposedAt, _ = time.Parse(time.RFC3339, decAt.String)
	}
	return &model.DecompMeta{
		ID: id, SourceID: sourceID, ModelID: modelID, DecomposedBy: decBy.String,
		DecomposedAt: decomposedAt, FactCount: factCount, SummaryCount: summaryCount,
		HasEmbeddings: embModelVal != 0, EmbeddingModel: embModel.String,
		EmbeddingDims: embDims, S3Key: s3key, CreatedAt: createdAt,
	}, nil
}

func scanDecomps(rows interface {
	Next() bool
	Scan(...any) error
	Close() error
	Err() error
}) ([]model.DecompMeta, error) {
	var out []model.DecompMeta
	for rows.Next() {
		m, err := scanDecomp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullTime(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *SQLiteStore) CreateUser(ctx context.Context, user *model.User) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id, email, password_hash, display_name, role, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		user.ID, user.Email, user.PasswordHash, user.DisplayName, user.Role,
		user.CreatedAt.UTC().Format(time.RFC3339), user.UpdatedAt.UTC().Format(time.RFC3339))
	return err
}

func (s *SQLiteStore) GetUserByEmail(ctx context.Context, email string) (*model.User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, email, password_hash, display_name, role, created_at, updated_at
		 FROM users WHERE email = ?`, email)
	return scanUser(row)
}

func (s *SQLiteStore) GetUserByID(ctx context.Context, id string) (*model.User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, email, password_hash, display_name, role, created_at, updated_at
		 FROM users WHERE id = ?`, id)
	return scanUser(row)
}

func (s *SQLiteStore) UpdateUserRole(ctx context.Context, id, role string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET role = ?, updated_at = ? WHERE id = ?`,
		role, time.Now().UTC().Format(time.RFC3339), id)
	return err
}

func (s *SQLiteStore) ListUsers(ctx context.Context) ([]model.User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, email, password_hash, display_name, role, created_at, updated_at
		 FROM users ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) CreateAPIToken(ctx context.Context, tok *model.APIToken) error {
	var expiresAt *string
	if tok.ExpiresAt != nil {
		s := tok.ExpiresAt.UTC().Format(time.RFC3339)
		expiresAt = &s
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO api_tokens (id, user_id, name, token_hash, scope, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		tok.ID, tok.UserID, tok.Name, tok.TokenHash, tok.Scope, nullTimePtr(expiresAt),
		tok.CreatedAt.UTC().Format(time.RFC3339))
	return err
}

func (s *SQLiteStore) GetAPITokenByHash(ctx context.Context, hash string) (*model.APIToken, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, name, token_hash, scope, expires_at, created_at
		 FROM api_tokens WHERE token_hash = ?`, hash)
	return scanAPIToken(row)
}

func (s *SQLiteStore) ListAPITokens(ctx context.Context, userID string) ([]model.APIToken, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, user_id, name, token_hash, scope, expires_at, created_at
		 FROM api_tokens WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.APIToken
	for rows.Next() {
		t, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) RevokeAPIToken(ctx context.Context, id, userID string) error {
	if userID == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE id = ?`, id)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM api_tokens WHERE id = ? AND user_id = ?`, id, userID)
	return err
}

func scanUser(row interface{ Scan(...any) error }) (*model.User, error) {
	var id, email, hash, displayName, role, ca, ua string
	if err := row.Scan(&id, &email, &hash, &displayName, &role, &ca, &ua); err != nil {
		return nil, err
	}
	createdAt, _ := time.Parse(time.RFC3339, ca)
	updatedAt, _ := time.Parse(time.RFC3339, ua)
	return &model.User{
		ID: id, Email: email, PasswordHash: hash, DisplayName: displayName,
		Role: role, CreatedAt: createdAt, UpdatedAt: updatedAt,
	}, nil
}

func scanAPIToken(row interface{ Scan(...any) error }) (*model.APIToken, error) {
	var id, userID, name, hash, scope, ca string
	var expiresAt sql.NullString
	if err := row.Scan(&id, &userID, &name, &hash, &scope, &expiresAt, &ca); err != nil {
		return nil, err
	}
	createdAt, _ := time.Parse(time.RFC3339, ca)
	var eat *time.Time
	if expiresAt.Valid {
		t, _ := time.Parse(time.RFC3339, expiresAt.String)
		eat = &t
	}
	return &model.APIToken{
		ID: id, UserID: userID, Name: name, TokenHash: hash,
		Scope: scope, ExpiresAt: eat, CreatedAt: createdAt,
	}, nil
}

func nullTimePtr(s *string) interface{} {
	if s == nil {
		return nil
	}
	return *s
}
