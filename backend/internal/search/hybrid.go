package search

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/concepts"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// Deps is the bundle of collaborators the hybrid service needs. It
// is a transport-agnostic subset of the HTTP handler's Deps: the
// per-request store.Queries (built by the handler from the
// per-repo pool), the qdrant store, the embedding provider, and the
// hybrid config. Any nil dependency disables hybrid mode for that
// call (the caller must check Available before invoking the
// hybrid path).
type Deps struct {
	Queries           *store.Queries
	Qdrant            *qdrantstore.Store
	EmbeddingProvider ai.EmbeddingProvider
	EmbeddingCfg      config.EmbeddingConfig
	Hybrid            config.SearchHybridConfig
}

// Available reports whether hybrid search can run for this call:
// the master switch is on, qdrant and the embedding provider are
// wired, the embedding model is configured, and the over-fetch
// multiplier is positive. The caller still must supply a non-empty
// query string — an empty query always runs the lexical path
// (handled by the caller before calling Available).
func (d Deps) Available() bool {
	return d.Hybrid.Enabled &&
		d.Qdrant != nil &&
		d.EmbeddingProvider != nil &&
		d.EmbeddingCfg.Model != "" &&
		d.Hybrid.OverFetchMultiplier > 0
}

// FactResult is the hybrid search output for facts: the rows in
// fused order (already paginated to [offset, offset+limit)), the
// lexical total (the count of facts matching the lexical filter,
// used by the page envelope), and a per-id fused-score map the
// caller can use to attach a `score` field to each row. SearchMode
// is "hybrid" when both channels ran, "lexical" when the hybrid
// path was unavailable or failed open (the rows are the lexical
// result without fusion).
type FactResult struct {
	Rows       []store.ListFactsByRepoWithSourceCountRow
	Total      int64
	ScoreByID  map[uuid.UUID]float64
	SearchMode string
}

// HybridFacts runs a hybrid (lexical + semantic) fact search. The
// query is embedded and queried against the Qdrant fact collection
// concurrently with a lexical over-fetch; the two channels are
// fused via RRF and the final page is sliced in Go. `total` is the
// lexical match count (the same value the lexical-only path would
// return) so the page envelope stays meaningful.
//
// Fail-open: any qdrant or embedding error is logged and the call
// returns the lexical-only result with SearchMode="lexical". The
// search never fails because a side channel is down.
//
// Caller responsibilities:
//   - Pass a non-empty `query` (empty query should run the lexical
//     path directly, not this function).
//   - Pass a `statusFilter` consistent with the caller's REST/MCP
//     semantics ("" means all, "stable"/"new"/"to_delete" filter).
//   - Pass a `sort` of "created_at" or "source_count" — used only by
//     the lexical over-fetch, not the fused ordering (the fused
//     order always wins once hybrid runs).
func HybridFacts(ctx context.Context, d Deps, repoID pgtype.UUID, query, statusFilter, sort string, limit, offset int) (FactResult, error) {
	if !d.Available() {
		return runLexicalFacts(ctx, d, repoID, query, statusFilter, sort, limit, offset, "lexical")
	}
	if err := validateUUID(repoID); err != nil {
		return FactResult{}, err
	}
	repoUUID, _ := uuid.FromBytes(repoID.Bytes[:])

	overFetch := limit * d.Hybrid.OverFetchMultiplier
	if overFetch < limit {
		overFetch = limit
	}
	// The lexical channel fetches with offset=0 so the fused
	// ranking sees the top candidates from page 0; we apply the
	// caller's offset to the fused list in Go. Cap the over-fetch
	// at a sane ceiling (200 = the REST maxPageSize) so a huge
	// limit doesn't multiply into an unreasonable SQL window.
	if overFetch > 200 {
		overFetch = 200
	}

	// Embed the query (single input). On any error, fail open to
	// lexical-only.
	embedResp, err := d.EmbeddingProvider.Embed(ctx, nil, ai.EmbeddingRequest{
		Model:  d.EmbeddingCfg.Model,
		Inputs: []string{query},
	})
	if err != nil {
		log.Printf("search.hybrid: embedding query failed, falling back to lexical: %v", err)
		return runLexicalFacts(ctx, d, repoID, query, statusFilter, sort, limit, offset, "lexical")
	}
	if len(embedResp.Embeddings) == 0 || len(embedResp.Embeddings[0]) == 0 {
		log.Printf("search.hybrid: embedding returned empty vector, falling back to lexical")
		return runLexicalFacts(ctx, d, repoID, query, statusFilter, sort, limit, offset, "lexical")
	}
	queryVec := embedResp.Embeddings[0]

	type lexResult struct {
		rows  []store.ListFactsByRepoWithSourceCountRow
		total int64
		err   error
	}
	type semResult struct {
		hits []qdrantstore.Hit
		err  error
	}
	lexCh := make(chan lexResult, 1)
	semCh := make(chan semResult, 1)

	go func() {
		rows, lerr := d.Queries.ListFactsByRepoWithSourceCount(ctx, store.ListFactsByRepoWithSourceCountParams{
			RepositoryID: repoID,
			Column2:      statusFilter,
			Column3:      query,
			Column4:      sort,
			Limit:        int32(overFetch),
			Offset:       0,
		})
		total := int64(0)
		if lerr == nil {
			t, terr := d.Queries.CountFactsByRepo(ctx, store.CountFactsByRepoParams{
				RepositoryID: repoID,
				Column2:      statusFilter,
				Column3:      query,
			})
			if terr != nil {
				lerr = terr
			} else {
				total = t
			}
		}
		lexCh <- lexResult{rows: rows, total: total, err: lerr}
	}()
	go func() {
		hits, serr := d.Qdrant.SearchSimilar(ctx, queryVec, repoUUID, uuid.Nil, d.Hybrid.MinScore, overFetch)
		semCh <- semResult{hits: hits, err: serr}
	}()

	lex := <-lexCh
	sem := <-semCh

	if lex.err != nil {
		return FactResult{}, lex.err
	}
	if sem.err != nil {
		log.Printf("search.hybrid: qdrant search failed, falling back to lexical: %v", sem.err)
		// Return the lexical page (we still have to apply the
		// caller's offset/limit since the lexical over-fetch ran
		// with offset=0).
		return finalizeLexicalPageFacts(lex.rows, lex.total, limit, offset, "lexical"), nil
	}

	// Build the lexical IDRank list from the lexical rows (rank
	// = position in the lexical ORDER BY). The ts_rank_cd is not
	// surfaced by ListFactsByRepoWithSourceCount (it orders by
	// created_at or source_count), so the lexical Score is 0.0
	// and the channel contributes only via rank. That's fine for
	// RRF — the rank is the signal, the score is for debugging.
	lexRanks := make([]IDRank, 0, len(lex.rows))
	lexByID := make(map[uuid.UUID]store.ListFactsByRepoWithSourceCountRow, len(lex.rows))
	for i, r := range lex.rows {
		id := pgUUIDToUUID(r.ID)
		if id == nil {
			continue
		}
		lexRanks = append(lexRanks, IDRank{ID: *id, Rank: i, Score: 0})
		lexByID[*id] = r
	}

	semRanks := make([]IDRank, 0, len(sem.hits))
	for i, h := range sem.hits {
		semRanks = append(semRanks, IDRank{ID: h.ID, Rank: i, Score: float64(h.Score)})
	}

	fused := RRF(lexRanks, semRanks, d.Hybrid.RRFK)

	// Fetch the rows for any fused IDs the lexical batch didn't
	// already return. The missing set is usually small (semantic-
	// only hits not in the lexical top-N).
	missing := make([]pgtype.UUID, 0)
	for _, r := range fused {
		if _, ok := lexByID[r.ID]; !ok {
			missing = append(missing, pgUUIDFromUUID(r.ID))
		}
	}
	if len(missing) > 0 {
		extra, merr := d.Queries.GetFactsByIDsForSearch(ctx, store.GetFactsByIDsForSearchParams{
			Ids:          missing,
			RepositoryID: repoID,
		})
		if merr != nil {
			log.Printf("search.hybrid: fetching missing fact rows failed, falling back to lexical: %v", merr)
			return finalizeLexicalPageFacts(lex.rows, lex.total, limit, offset, "lexical"), nil
		}
		for _, r := range extra {
			id := pgUUIDToUUID(r.ID)
			if id == nil {
				continue
			}
			lexByID[*id] = store.ListFactsByRepoWithSourceCountRow{
				ID:            r.ID,
				Text:          r.Text,
				Status:        r.Status,
				EmbeddedAt:    r.EmbeddedAt,
				EmbeddedModel:  r.EmbeddedModel,
				CreatedAt:     r.CreatedAt,
				FactKind:      r.FactKind,
				ImageUrl:      r.ImageUrl,
				SourceCount:   r.SourceCount,
				SourceID:      r.SourceID,
			}
		}
	}

	// Apply the caller's offset/limit to the fused list.
	pageSize := limit
	if pageSize < 0 {
		pageSize = 0
	}
	start := offset
	if start < 0 {
		start = 0
	}
	if start > len(fused) {
		start = len(fused)
	}
	end := start + pageSize
	if end > len(fused) {
		end = len(fused)
	}
	page := fused[start:end]

	rows := make([]store.ListFactsByRepoWithSourceCountRow, 0, len(page))
	scoreByID := make(map[uuid.UUID]float64, len(page))
	for _, r := range page {
		row, ok := lexByID[r.ID]
		if !ok {
			// Shouldn't happen: every fused id is either in
			// the lexical batch or was fetched above. Skip to
			// defend against a stale Qdrant point whose fact
			// was deleted from Postgres.
			continue
		}
		rows = append(rows, row)
		scoreByID[r.ID] = r.FusedScore
	}
	return FactResult{
		Rows:       rows,
		Total:      lex.total,
		ScoreByID:  scoreByID,
		SearchMode: "hybrid",
	}, nil
}

// runLexicalFacts is the fail-open / not-available path: run the
// caller's exact lexical page (with their offset/limit) and tag the
// result with the given search mode.
func runLexicalFacts(ctx context.Context, d Deps, repoID pgtype.UUID, query, statusFilter, sort string, limit, offset int, mode string) (FactResult, error) {
	rows, err := d.Queries.ListFactsByRepoWithSourceCount(ctx, store.ListFactsByRepoWithSourceCountParams{
		RepositoryID: repoID,
		Column2:      statusFilter,
		Column3:      query,
		Column4:      sort,
		Limit:        int32(limit),
		Offset:       int32(offset),
	})
	if err != nil {
		return FactResult{}, err
	}
	total, err := d.Queries.CountFactsByRepo(ctx, store.CountFactsByRepoParams{
		RepositoryID: repoID,
		Column2:      statusFilter,
		Column3:      query,
	})
	if err != nil {
		return FactResult{}, err
	}
	return FactResult{Rows: rows, Total: total, SearchMode: mode}, nil
}

// finalizeLexicalPageFacts slices an over-fetched lexical batch to
// the caller's offset/limit and wraps it as a FactResult. Used when
// the semantic channel failed mid-hybrid and we already paid for
// the over-fetch.
func finalizeLexicalPageFacts(rows []store.ListFactsByRepoWithSourceCountRow, total int64, limit, offset int, mode string) FactResult {
	start := offset
	if start < 0 {
		start = 0
	}
	if start > len(rows) {
		start = len(rows)
	}
	end := start + limit
	if end > len(rows) {
		end = len(rows)
	}
	if limit < 0 {
		end = start
	}
	return FactResult{
		Rows:       rows[start:end],
		Total:      total,
		SearchMode: mode,
	}
}

// ConceptResult is the hybrid search output for concepts. The
// groups are built in fused order (already paginated to
// [offset, offset+limit)). `total` is the lexical group count
// (distinct lower(canonical_name)) for the page envelope. Each
// group's SearchRank is overridden with the MAX FusedScore across
// its contexts so the existing sortGroups tie-break keeps the
// fused order.
type ConceptResult struct {
	Groups     []concepts.Group
	Total      int64
	ScoreByID  map[uuid.UUID]float64
	SearchMode string
}

// HybridConcepts runs a hybrid concept search. The query is embedded
// and queried against the Qdrant concept collection concurrently
// with a lexical over-fetch of the grouped-concepts query; the two
// channels are fused via RRF at the concept_id level. The fused
// concept_ids are then grouped by lower(canonical_name) as in the
// lexical path (concepts.BuildGroups), with each group's
// SearchRank set to the MAX FusedScore across its contexts.
//
// Fail-open: qdrant/embedding errors log and return the lexical
// result.
func HybridConcepts(ctx context.Context, d Deps, repoID pgtype.UUID, query string, limit, offset int) (ConceptResult, error) {
	if !d.Available() {
		return runLexicalConcepts(ctx, d, repoID, query, limit, offset, "lexical")
	}
	if err := validateUUID(repoID); err != nil {
		return ConceptResult{}, err
	}
	repoUUID, _ := uuid.FromBytes(repoID.Bytes[:])

	overFetch := limit * d.Hybrid.OverFetchMultiplier
	if overFetch < limit {
		overFetch = limit
	}
	// Concepts list returns the full set in SQL and paginates the
	// groups in Go (see ListConcepts handler). For the over-fetch,
	// we fetch overFetch candidate rows (NOT groups) from each
	// channel — that's a generous upper bound on the number of
	// groups the fused page will see. Cap at 500 to bound work.
	if overFetch > 500 {
		overFetch = 500
	}

	embedResp, err := d.EmbeddingProvider.Embed(ctx, nil, ai.EmbeddingRequest{
		Model:  d.EmbeddingCfg.Model,
		Inputs: []string{query},
	})
	if err != nil {
		log.Printf("search.hybrid: embedding query failed, falling back to lexical: %v", err)
		return runLexicalConcepts(ctx, d, repoID, query, limit, offset, "lexical")
	}
	if len(embedResp.Embeddings) == 0 || len(embedResp.Embeddings[0]) == 0 {
		log.Printf("search.hybrid: embedding returned empty vector, falling back to lexical")
		return runLexicalConcepts(ctx, d, repoID, query, limit, offset, "lexical")
	}
	queryVec := embedResp.Embeddings[0]

	type lexResult struct {
		rows  []store.ListGroupedConceptsByRepoRow
		total int64
		err   error
	}
	type semResult struct {
		hits []qdrantstore.Hit
		err  error
	}
	lexCh := make(chan lexResult, 1)
	semCh := make(chan semResult, 1)

	go func() {
		rows, lerr := d.Queries.ListGroupedConceptsByRepo(ctx, store.ListGroupedConceptsByRepoParams{
			RepositoryID: repoID,
			Q:            query,
		})
		total := int64(0)
		if lerr == nil {
			t, terr := d.Queries.CountGroupedConceptsByRepo(ctx, store.CountGroupedConceptsByRepoParams{
				RepositoryID: repoID,
				Q:            query,
			})
			if terr != nil {
				lerr = terr
			} else {
				total = t
			}
		}
		lexCh <- lexResult{rows: rows, total: total, err: lerr}
	}()
	go func() {
		hits, serr := d.Qdrant.SearchSimilarConcepts(ctx, queryVec, repoUUID, d.Hybrid.MinScore, overFetch)
		semCh <- semResult{hits: hits, err: serr}
	}()

	lex := <-lexCh
	sem := <-semCh

	if lex.err != nil {
		return ConceptResult{}, lex.err
	}
	if sem.err != nil {
		log.Printf("search.hybrid: qdrant concept search failed, falling back to lexical: %v", sem.err)
		return finalizeLexicalPageConcepts(lex.rows, lex.total, limit, offset, "lexical"), nil
	}

	lexRanks := make([]IDRank, 0, len(lex.rows))
	lexByID := make(map[uuid.UUID]store.ListGroupedConceptsByRepoRow, len(lex.rows))
	for i, r := range lex.rows {
		id := pgUUIDToUUID(r.ID)
		if id == nil {
			continue
		}
		// The lexical score is MAX(name_rank, alias_rank) — the
		// same combination BuildGroups uses — so the RRF
		// contribution is meaningful relative to the semantic
		// cosine score.
		score := float64(r.NameRank)
		if r.AliasRank > r.NameRank {
			score = float64(r.AliasRank)
		}
		lexRanks = append(lexRanks, IDRank{ID: *id, Rank: i, Score: score})
		lexByID[*id] = r
	}

	semRanks := make([]IDRank, 0, len(sem.hits))
	for i, h := range sem.hits {
		semRanks = append(semRanks, IDRank{ID: h.ID, Rank: i, Score: float64(h.Score)})
	}

	fused := RRF(lexRanks, semRanks, d.Hybrid.RRFK)

	missing := make([]pgtype.UUID, 0)
	for _, r := range fused {
		if _, ok := lexByID[r.ID]; !ok {
			missing = append(missing, pgUUIDFromUUID(r.ID))
		}
	}
	if len(missing) > 0 {
		extra, merr := d.Queries.GetConceptsByIDsForSearch(ctx, store.GetConceptsByIDsForSearchParams{
			Ids:          missing,
			RepositoryID: repoID,
		})
		if merr != nil {
			log.Printf("search.hybrid: fetching missing concept rows failed, falling back to lexical: %v", merr)
			return finalizeLexicalPageConcepts(lex.rows, lex.total, limit, offset, "lexical"), nil
		}
		for _, r := range extra {
			id := pgUUIDToUUID(r.ID)
			if id == nil {
				continue
			}
			lexByID[*id] = store.ListGroupedConceptsByRepoRow{
				ID:            r.ID,
				RepositoryID:  r.RepositoryID,
				CanonicalName:  r.CanonicalName,
				Context:       r.Context,
				Description:    r.Description,
				EmbeddedAt:     r.EmbeddedAt,
				EmbeddedModel:  r.EmbeddedModel,
				CreatedAt:      r.CreatedAt,
				FactCount:      r.FactCount,
				NameRank:       r.NameRank,
				AliasRank:      r.AliasRank,
			}
		}
	}

	// Build the row set in fused order; BuildGroups groups by
	// lower(canonical_name) and reorders by SearchRank DESC, so
	// we set each row's NameRank to the FusedScore (and AliasRank
	// to 0) — that makes searchRank() return FusedScore, and the
	// group's SearchRank (the MAX across contexts) is the MAX
	// FusedScore across the group's contexts. The pre-existing
	// sortGroups tie-break (SearchRank DESC, TotalFactCount DESC,
	// CanonicalName ASC) keeps the fused order while staying
	// stable on ties.
	scoreByID := make(map[uuid.UUID]float64, len(fused))
	groupRows := make([]concepts.GroupRow, 0, len(fused))
	for _, r := range fused {
		row, ok := lexByID[r.ID]
		if !ok {
			continue
		}
		scoreByID[r.ID] = r.FusedScore
		groupRows = append(groupRows, concepts.GroupRow{
			ID:            row.ID,
			CanonicalName: row.CanonicalName,
			Context:       row.Context,
			Description:   row.Description,
			EmbeddedAt:    row.EmbeddedAt,
			EmbeddedModel: row.EmbeddedModel,
			CreatedAt:     row.CreatedAt,
			FactCount:     row.FactCount,
			NameRank:      float32(r.FusedScore),
			AliasRank:     0,
		})
	}
	groups := concepts.BuildGroups(groupRows, nil)
	page := concepts.Paginate(groups, offset, limit)
	return ConceptResult{
		Groups:     page,
		Total:      lex.total,
		ScoreByID:  scoreByID,
		SearchMode: "hybrid",
	}, nil
}

func runLexicalConcepts(ctx context.Context, d Deps, repoID pgtype.UUID, query string, limit, offset int, mode string) (ConceptResult, error) {
	rows, err := d.Queries.ListGroupedConceptsByRepo(ctx, store.ListGroupedConceptsByRepoParams{
		RepositoryID: repoID,
		Q:            query,
	})
	if err != nil {
		return ConceptResult{}, err
	}
	total, err := d.Queries.CountGroupedConceptsByRepo(ctx, store.CountGroupedConceptsByRepoParams{
		RepositoryID: repoID,
		Q:            query,
	})
	if err != nil {
		return ConceptResult{}, err
	}
	groupRows := make([]concepts.GroupRow, 0, len(rows))
	for _, r := range rows {
		groupRows = append(groupRows, concepts.FromListGroupedConceptsByRepoRow(r))
	}
	groups := concepts.BuildGroups(groupRows, nil)
	page := concepts.Paginate(groups, offset, limit)
	return ConceptResult{Groups: page, Total: total, SearchMode: mode}, nil
}

func finalizeLexicalPageConcepts(rows []store.ListGroupedConceptsByRepoRow, total int64, limit, offset int, mode string) ConceptResult {
	groupRows := make([]concepts.GroupRow, 0, len(rows))
	for _, r := range rows {
		groupRows = append(groupRows, concepts.FromListGroupedConceptsByRepoRow(r))
	}
	groups := concepts.BuildGroups(groupRows, nil)
	page := concepts.Paginate(groups, offset, limit)
	return ConceptResult{Groups: page, Total: total, SearchMode: mode}
}

// validateUUID returns an error if the pgtype.UUID is not in its
// valid (scanned) form. Used to guard the uuid.FromBytes call that
// builds the Qdrant repository filter.
func validateUUID(id pgtype.UUID) error {
	if !id.Valid || len(id.Bytes) != 16 {
		return errors.New("search.hybrid: invalid repository uuid")
	}
	return nil
}

// pgUUIDToUUID converts a pgtype.UUID to a *uuid.UUID, returning nil
// if the input is invalid. Centralizes the conversion so the hybrid
// service doesn't repeat the validity check.
func pgUUIDToUUID(id pgtype.UUID) *uuid.UUID {
	if !id.Valid || len(id.Bytes) != 16 {
		return nil
	}
	u, err := uuid.FromBytes(id.Bytes[:])
	if err != nil {
		return nil
	}
	return &u
}

// pgUUIDFromUUID is the inverse of pgUUIDToUUID.
func pgUUIDFromUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

// EmbedTimeout is the deadline applied to the embedding call when
// the caller's context doesn't already carry one. Kept generous so
// a slow provider doesn't trip fail-open unnecessarily.
const EmbedTimeout = 30 * time.Second