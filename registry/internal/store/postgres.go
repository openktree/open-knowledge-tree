package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openktree/knowledge-registry/internal/model"
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

func (s *PostgresStore) Close() error {
	s.pool.Close()
	return nil
}

func (s *PostgresStore) CreateRepository(ctx context.Context, repo *model.Repository) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO repositories (id, name, description, owner, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		repo.ID, repo.Name, repo.Description, repo.Owner, repo.CreatedAt, repo.UpdatedAt)
	return err
}

func (s *PostgresStore) GetRepository(ctx context.Context, id string) (*model.Repository, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, name, description, owner, created_at, updated_at FROM repositories WHERE id = $1`, id)
	return scanRepoPG(row)
}

func (s *PostgresStore) ListRepositories(ctx context.Context) ([]model.Repository, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, description, owner, created_at, updated_at FROM repositories ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Repository
	for rows.Next() {
		r, err := scanRepoPG(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func (s *PostgresStore) UpdateRepository(ctx context.Context, id string, upd model.RepoUpdate) error {
	if upd.Name != nil {
		if _, err := s.pool.Exec(ctx,
			`UPDATE repositories SET name = $1, updated_at = now() WHERE id = $2`,
			*upd.Name, id); err != nil {
			return err
		}
	}
	if upd.Description != nil {
		if _, err := s.pool.Exec(ctx,
			`UPDATE repositories SET description = $1, updated_at = now() WHERE id = $2`,
			*upd.Description, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *PostgresStore) DeleteRepository(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM repositories WHERE id = $1`, id)
	return err
}

func (s *PostgresStore) IndexSource(ctx context.Context, meta *model.SourceMeta) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO sources (id, repo_id, url, doi, sha256, title, s3_key, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 ON CONFLICT(id) DO UPDATE SET
		   url=EXCLUDED.url, doi=EXCLUDED.doi, sha256=EXCLUDED.sha256,
		   title=EXCLUDED.title, s3_key=EXCLUDED.s3_key, updated_at=EXCLUDED.updated_at`,
		meta.ID, meta.RepoID, nullStringPtr(meta.URL), nullStringPtr(meta.DOI),
		nullStringPtr(meta.SHA256), nullStringPtr(meta.Title), meta.S3Key,
		meta.CreatedAt, meta.UpdatedAt)
	return err
}

func (s *PostgresStore) GetSource(ctx context.Context, repoID, sourceID string) (*model.SourceMeta, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, repo_id, url, doi, sha256, title, s3_key, created_at, updated_at
		 FROM sources WHERE id = $1 AND repo_id = $2`, sourceID, repoID)
	return scanSourcePG(row)
}

func (s *PostgresStore) ListAllSources(ctx context.Context, limit, offset int) ([]model.SourceMeta, error) {
	if limit <= 0 || limit > 500 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, repo_id, url, doi, sha256, title, s3_key, created_at, updated_at
		 FROM sources ORDER BY created_at DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSourcesPG(rows)
}

func (s *PostgresStore) ListSources(ctx context.Context, repoID string, limit, offset int) ([]model.SourceMeta, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, repo_id, url, doi, sha256, title, s3_key, created_at, updated_at
		 FROM sources WHERE repo_id = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3`,
		repoID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSourcesPG(rows)
}

func (s *PostgresStore) SearchByURL(ctx context.Context, repoID, url string) ([]model.SourceMeta, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, repo_id, url, doi, sha256, title, s3_key, created_at, updated_at
		 FROM sources WHERE repo_id = $1 AND url = $2`, repoID, url)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSourcesPG(rows)
}

func (s *PostgresStore) SearchByDOI(ctx context.Context, repoID, doi string) ([]model.SourceMeta, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, repo_id, url, doi, sha256, title, s3_key, created_at, updated_at
		 FROM sources WHERE repo_id = $1 AND doi = $2`, repoID, doi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSourcesPG(rows)
}

func (s *PostgresStore) SearchBySHA256(ctx context.Context, repoID, sha256 string) ([]model.SourceMeta, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, repo_id, url, doi, sha256, title, s3_key, created_at, updated_at
		 FROM sources WHERE repo_id = $1 AND sha256 = $2`, repoID, sha256)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSourcesPG(rows)
}

func (s *PostgresStore) IndexDecomposition(ctx context.Context, meta *model.DecompMeta) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO decompositions (id, source_id, model_id, decomposed_by, decomposed_at,
		   fact_count, summary_count, has_embeddings, embedding_model, embedding_dims, s3_key, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		 ON CONFLICT(id) DO UPDATE SET
		   fact_count=EXCLUDED.fact_count, summary_count=EXCLUDED.summary_count,
		   has_embeddings=EXCLUDED.has_embeddings, s3_key=EXCLUDED.s3_key`,
		meta.ID, meta.SourceID, meta.ModelID, meta.DecomposedBy,
		nullTimePtrPG(meta.DecomposedAt), meta.FactCount, meta.SummaryCount,
		meta.HasEmbeddings, nullStringPtr(meta.EmbeddingModel), meta.EmbeddingDims,
		meta.S3Key, meta.CreatedAt)
	return err
}

func (s *PostgresStore) ListDecompositions(ctx context.Context, sourceID string) ([]model.DecompMeta, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, source_id, model_id, decomposed_by, decomposed_at,
		   fact_count, summary_count, has_embeddings, embedding_model, embedding_dims, s3_key, created_at
		 FROM decompositions WHERE source_id = $1 ORDER BY decomposed_at DESC`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDecompsPG(rows)
}

func (s *PostgresStore) GetDecompositionBySourceAndModel(ctx context.Context, sourceID, modelID string) (*model.DecompMeta, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, source_id, model_id, decomposed_by, decomposed_at,
		   fact_count, summary_count, has_embeddings, embedding_model, embedding_dims, s3_key, created_at
		 FROM decompositions WHERE source_id = $1 AND model_id = $2`, sourceID, modelID)
	return scanDecompPG(row)
}

func (s *PostgresStore) ListAllDecompositions(ctx context.Context, limit, offset int) ([]model.DecompMeta, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, source_id, model_id, decomposed_by, decomposed_at,
		   fact_count, summary_count, has_embeddings, embedding_model, embedding_dims, s3_key, created_at
		 FROM decompositions ORDER BY created_at ASC LIMIT $1 OFFSET $2`,
		limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDecompsPG(rows)
}

func (s *PostgresStore) UpdateDecompositionEmbeddingModel(ctx context.Context, id, embeddingModel string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE decompositions SET embedding_model = $1 WHERE id = $2`,
		embeddingModel, id)
	return err
}

func (s *PostgresStore) FactHashExists(ctx context.Context, sourceID, hash string) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM fact_hashes WHERE content_hash = $1 AND source_id = $2`, hash, sourceID).Scan(&n)
	return n > 0, err
}

func (s *PostgresStore) InsertFactHash(ctx context.Context, hash, sourceID, decompID, factID string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO fact_hashes (content_hash, source_id, decomposition_id, fact_id, created_at)
		 VALUES ($1, $2, $3, $4, now())`,
		hash, sourceID, decompID, factID)
	return err
}

func (s *PostgresStore) LinkFactHash(ctx context.Context, hash, sourceID, decompID string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO fact_hashes (content_hash, source_id, decomposition_id, fact_id, created_at)
		 VALUES ($1, $2, $3, '', now())
		 ON CONFLICT DO NOTHING`,
		hash, sourceID, decompID)
	return err
}

// BatchUpsertFactHashes inserts new fact hashes and re-links existing
// ones in a single transaction. Replaces the per-fact loop of
// FactHashExists + InsertFactHash/LinkFactHash (2N queries) with
// one tx: one SELECT ... = ANY($1) + batch INSERT, all committed
// together. For 200 facts this cuts ~400 queries down to 2.
func (s *PostgresStore) BatchUpsertFactHashes(ctx context.Context, sourceID, decompID string, entries []model.FactHashEntry) (model.BatchFactHashResult, error) {
	if len(entries) == 0 {
		return model.BatchFactHashResult{}, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return model.BatchFactHashResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Batch SELECT: which hashes already exist for this source?
	hashes := make([]string, len(entries))
	for i, e := range entries {
		hashes[i] = e.ContentHash
	}
	rows, err := tx.Query(ctx,
		`SELECT content_hash FROM fact_hashes WHERE source_id = $1 AND content_hash = ANY($2)`,
		sourceID, hashes)
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
	now := time.Now().UTC()
	var newCount, linkedCount int
	for _, e := range entries {
		if existingSet[e.ContentHash] {
			if _, err := tx.Exec(ctx,
				`INSERT INTO fact_hashes (content_hash, source_id, decomposition_id, fact_id, created_at)
				 VALUES ($1, $2, $3, '', $4)
				 ON CONFLICT DO NOTHING`,
				e.ContentHash, sourceID, decompID, now); err != nil {
				return model.BatchFactHashResult{}, fmt.Errorf("linking fact hash %s: %w", e.ContentHash, err)
			}
			linkedCount++
		} else {
			if _, err := tx.Exec(ctx,
				`INSERT INTO fact_hashes (content_hash, source_id, decomposition_id, fact_id, created_at)
				 VALUES ($1, $2, $3, $4, $5)`,
				e.ContentHash, sourceID, decompID, e.FactID, now); err != nil {
				return model.BatchFactHashResult{}, fmt.Errorf("inserting fact hash %s: %w", e.ContentHash, err)
			}
			existingSet[e.ContentHash] = true
			newCount++
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return model.BatchFactHashResult{}, fmt.Errorf("commit batch fact hashes: %w", err)
	}
	return model.BatchFactHashResult{New: newCount, Linked: linkedCount}, nil
}

func (s *PostgresStore) SearchByText(ctx context.Context, repoID, query string, limit, offset int) ([]model.SourceMeta, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	pattern := "%" + query + "%"
	rows, err := s.pool.Query(ctx,
		`SELECT id, repo_id, url, doi, sha256, title, s3_key, created_at, updated_at
		 FROM sources WHERE repo_id = $1 AND (title ILIKE $2 OR url ILIKE $3 OR doi ILIKE $4)
		 ORDER BY created_at DESC LIMIT $5 OFFSET $6`,
		repoID, pattern, pattern, pattern, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSourcesPG(rows)
}

func (s *PostgresStore) CountByText(ctx context.Context, repoID, query string) (int, error) {
	pattern := "%" + query + "%"
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM sources WHERE repo_id = $1 AND (title ILIKE $2 OR url ILIKE $3 OR doi ILIKE $4)`,
		repoID, pattern, pattern, pattern).Scan(&n)
	return n, err
}

func (s *PostgresStore) CountAllSources(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM sources`).Scan(&n)
	return n, err
}

func (s *PostgresStore) Stats(ctx context.Context) (repoCount, sourceCount int, err error) {
	err = s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM repositories`).Scan(&repoCount)
	if err != nil {
		return 0, 0, err
	}
	err = s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM sources`).Scan(&sourceCount)
	return
}

// ── Graphs ──────────────────────────────────────────────────────────

func (s *PostgresStore) IndexGraph(ctx context.Context, meta *model.GraphMeta) error {
	tags := meta.Tags
	if tags == nil {
		tags = []string{}
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO graphs (id, name, description, owner, tags, source_count, fact_count, concept_count, s3_key, sha256, schema_version, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		 ON CONFLICT(id) DO UPDATE SET
		   name=EXCLUDED.name, description=EXCLUDED.description, owner=EXCLUDED.owner,
		   tags=EXCLUDED.tags, source_count=EXCLUDED.source_count, fact_count=EXCLUDED.fact_count,
		   concept_count=EXCLUDED.concept_count, s3_key=EXCLUDED.s3_key, sha256=EXCLUDED.sha256,
		   schema_version=EXCLUDED.schema_version, updated_at=EXCLUDED.updated_at`,
		meta.ID, meta.Name, meta.Description, meta.Owner, tags,
		meta.SourceCount, meta.FactCount, meta.ConceptCount, meta.S3Key, meta.SHA256,
		meta.SchemaVersion, meta.CreatedAt, meta.UpdatedAt)
	return err
}

func (s *PostgresStore) GetGraph(ctx context.Context, id string) (*model.GraphMeta, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, name, description, owner, tags, source_count, fact_count, concept_count, s3_key, sha256, schema_version, created_at, updated_at
		 FROM graphs WHERE id = $1`, id)
	return scanGraphPG(row)
}

func (s *PostgresStore) ListGraphs(ctx context.Context, limit, offset int) ([]model.GraphMeta, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, description, owner, tags, source_count, fact_count, concept_count, s3_key, sha256, schema_version, created_at, updated_at
		 FROM graphs ORDER BY created_at DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGraphsPG(rows)
}

func (s *PostgresStore) SearchGraphsByText(ctx context.Context, query string, limit, offset int) ([]model.GraphMeta, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	pattern := "%" + query + "%"
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, description, owner, tags, source_count, fact_count, concept_count, s3_key, sha256, schema_version, created_at, updated_at
		 FROM graphs WHERE name ILIKE $1 OR description ILIKE $2
		 ORDER BY created_at DESC LIMIT $3 OFFSET $4`,
		pattern, pattern, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGraphsPG(rows)
}

func (s *PostgresStore) SearchGraphsByTag(ctx context.Context, tag string, limit, offset int) ([]model.GraphMeta, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, description, owner, tags, source_count, fact_count, concept_count, s3_key, sha256, schema_version, created_at, updated_at
		 FROM graphs WHERE $1 = ANY(tags)
		 ORDER BY created_at DESC LIMIT $2 OFFSET $3`,
		tag, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGraphsPG(rows)
}

func (s *PostgresStore) CountGraphs(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM graphs`).Scan(&n)
	return n, err
}

func (s *PostgresStore) CountGraphsByText(ctx context.Context, query string) (int, error) {
	pattern := "%" + query + "%"
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM graphs WHERE name ILIKE $1 OR description ILIKE $2`,
		pattern, pattern).Scan(&n)
	return n, err
}

func (s *PostgresStore) CountGraphsByTag(ctx context.Context, tag string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM graphs WHERE $1 = ANY(tags)`, tag).Scan(&n)
	return n, err
}

func (s *PostgresStore) DeleteGraph(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM graphs WHERE id = $1`, id)
	return err
}

func (s *PostgresStore) UpsertContext(ctx context.Context, label, description string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO contexts (label, description, created_at, updated_at)
		 VALUES ($1, $2, now(), now())
		 ON CONFLICT(label) DO UPDATE SET
		   description=EXCLUDED.description, updated_at=now()`,
		label, description)
	return err
}

func (s *PostgresStore) ListContexts(ctx context.Context) ([]model.ContextClass, error) {
	rows, err := s.pool.Query(ctx,
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

func (s *PostgresStore) CreateUser(ctx context.Context, user *model.User) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO users (id, email, password_hash, display_name, role, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		user.ID, user.Email, user.PasswordHash, user.DisplayName, user.Role,
		user.CreatedAt, user.UpdatedAt)
	return err
}

func (s *PostgresStore) GetUserByEmail(ctx context.Context, email string) (*model.User, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, display_name, role, created_at, updated_at
		 FROM users WHERE email = $1`, email)
	return scanUserPG(row)
}

func (s *PostgresStore) GetUserByID(ctx context.Context, id string) (*model.User, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, display_name, role, created_at, updated_at
		 FROM users WHERE id = $1`, id)
	return scanUserPG(row)
}

func (s *PostgresStore) UpdateUserRole(ctx context.Context, id, role string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE users SET role = $1, updated_at = now() WHERE id = $2`, role, id)
	return err
}

func (s *PostgresStore) ListUsers(ctx context.Context) ([]model.User, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, email, password_hash, display_name, role, created_at, updated_at
		 FROM users ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.User
	for rows.Next() {
		u, err := scanUserPG(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

func (s *PostgresStore) CreateAPIToken(ctx context.Context, tok *model.APIToken) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO api_tokens (id, user_id, name, token_hash, scope, expires_at, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		tok.ID, tok.UserID, tok.Name, tok.TokenHash, tok.Scope, tok.ExpiresAt, tok.CreatedAt)
	return err
}

func (s *PostgresStore) GetAPITokenByHash(ctx context.Context, hash string) (*model.APIToken, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, user_id, name, token_hash, scope, expires_at, created_at
		 FROM api_tokens WHERE token_hash = $1`, hash)
	return scanAPITokenPG(row)
}

func (s *PostgresStore) ListAPITokens(ctx context.Context, userID string) ([]model.APIToken, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, user_id, name, token_hash, scope, expires_at, created_at
		 FROM api_tokens WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.APIToken
	for rows.Next() {
		t, err := scanAPITokenPG(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (s *PostgresStore) RevokeAPIToken(ctx context.Context, id, userID string) error {
	if userID == "" {
		_, err := s.pool.Exec(ctx, `DELETE FROM api_tokens WHERE id = $1`, id)
		return err
	}
	_, err := s.pool.Exec(ctx,
		`DELETE FROM api_tokens WHERE id = $1 AND user_id = $2`, id, userID)
	return err
}

var _ MetadataStore = (*PostgresStore)(nil)
var _ MetadataStore = (*SQLiteStore)(nil)

func nullStringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullTimePtrPG(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

type pgxRows interface {
	Next() bool
	Scan(dest ...any) error
	Close()
	Err() error
}

func scanRepoPG(row interface{ Scan(...any) error }) (*model.Repository, error) {
	var id, name, desc, owner string
	var ca, ua time.Time
	if err := row.Scan(&id, &name, &desc, &owner, &ca, &ua); err != nil {
		return nil, err
	}
	return &model.Repository{
		ID: id, Name: name, Description: desc, Owner: owner,
		CreatedAt: ca, UpdatedAt: ua,
	}, nil
}

func scanSourcePG(row interface{ Scan(...any) error }) (*model.SourceMeta, error) {
	var id, repoID, s3key string
	var ca, ua time.Time
	var url, doi, sha256, title *string
	if err := row.Scan(&id, &repoID, &url, &doi, &sha256, &title, &s3key, &ca, &ua); err != nil {
		return nil, err
	}
	return &model.SourceMeta{
		ID: id, RepoID: repoID,
		URL: ptrStr(url), DOI: ptrStr(doi),
		SHA256: ptrStr(sha256), Title: ptrStr(title), S3Key: s3key,
		CreatedAt: ca, UpdatedAt: ua,
	}, nil
}

func scanSourcesPG(rows pgxRows) ([]model.SourceMeta, error) {
	var out []model.SourceMeta
	for rows.Next() {
		m, err := scanSourcePG(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func scanDecompPG(row interface{ Scan(...any) error }) (*model.DecompMeta, error) {
	var id, sourceID, modelID, s3key string
	var ca time.Time
	var decBy, embModel *string
	var decAt *time.Time
	var factCount, summaryCount, embDims int
	var hasEmb bool
	if err := row.Scan(&id, &sourceID, &modelID, &decBy, &decAt,
		&factCount, &summaryCount, &hasEmb, &embModel, &embDims, &s3key, &ca); err != nil {
		return nil, err
	}
	decAtVal := time.Time{}
	if decAt != nil {
		decAtVal = *decAt
	}
	return &model.DecompMeta{
		ID: id, SourceID: sourceID, ModelID: modelID, DecomposedBy: ptrStr(decBy),
		DecomposedAt: decAtVal, FactCount: factCount, SummaryCount: summaryCount,
		HasEmbeddings: hasEmb, EmbeddingModel: ptrStr(embModel),
		EmbeddingDims: embDims, S3Key: s3key, CreatedAt: ca,
	}, nil
}

func scanDecompsPG(rows pgxRows) ([]model.DecompMeta, error) {
	var out []model.DecompMeta
	for rows.Next() {
		m, err := scanDecompPG(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func scanUserPG(row interface{ Scan(...any) error }) (*model.User, error) {
	var id, email, hash, displayName, role string
	var ca, ua time.Time
	if err := row.Scan(&id, &email, &hash, &displayName, &role, &ca, &ua); err != nil {
		return nil, err
	}
	return &model.User{
		ID: id, Email: email, PasswordHash: hash, DisplayName: displayName,
		Role: role, CreatedAt: ca, UpdatedAt: ua,
	}, nil
}

func scanAPITokenPG(row interface{ Scan(...any) error }) (*model.APIToken, error) {
	var id, userID, name, hash, scope string
	var ca time.Time
	var expiresAt *time.Time
	if err := row.Scan(&id, &userID, &name, &hash, &scope, &expiresAt, &ca); err != nil {
		return nil, err
	}
	return &model.APIToken{
		ID: id, UserID: userID, Name: name, TokenHash: hash,
		Scope: scope, ExpiresAt: expiresAt, CreatedAt: ca,
	}, nil
}

func ptrStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func scanGraphPG(row interface{ Scan(...any) error }) (*model.GraphMeta, error) {
	var id, name, desc, owner, s3key, sha string
	var tags []string
	var srcCount, factCount, conceptCount, schemaVersion int
	var ca, ua time.Time
	if err := row.Scan(&id, &name, &desc, &owner, &tags, &srcCount, &factCount, &conceptCount, &s3key, &sha, &schemaVersion, &ca, &ua); err != nil {
		return nil, err
	}
	return &model.GraphMeta{
		ID: id, Name: name, Description: desc, Owner: owner, Tags: tags,
		SourceCount: srcCount, FactCount: factCount, ConceptCount: conceptCount,
		S3Key: s3key, SHA256: sha, SchemaVersion: schemaVersion,
		CreatedAt: ca, UpdatedAt: ua,
	}, nil
}

func scanGraphsPG(rows pgxRows) ([]model.GraphMeta, error) {
	var out []model.GraphMeta
	for rows.Next() {
		m, err := scanGraphPG(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}
