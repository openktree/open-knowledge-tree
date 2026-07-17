package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/google/uuid"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	appmw "github.com/openktree/open-knowledge-tree/backend/internal/api/middleware"
	"github.com/openktree/open-knowledge-tree/backend/internal/concepts"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// Concepts bundles the concept HTTP handlers. A concept is a
// repo-scoped, canonical-named entity assigned a context (an L3
// ontology class label). Concepts are produced by the
// extract_concepts worker (chained after dedup); this handler is
// the read-side surface.
//
// At the API level, concepts are unified by canonical name: every
// per-context row sharing the same lower(canonical_name) within a
// repo collapses into one concept group with multiple contexts. So
// the list endpoint returns one entry per canonical name (with a
// contexts array), and the detail endpoint (addressed by a concept
// UUID, resolved to its canonical_name group) returns the whole
// group. Facts stay linked to a specific per-context concept_id, so
// GET /concepts/{conceptID}/facts still returns that context's facts
// only — facts are compartmentalized per context.
//
// All handlers are repo-scoped: they read the per-request pool and
// repository UUID from the context set by WithRepoQueries, the
// same way the source and investigation handlers do. Read
// endpoints require only authentication; concept creation is
// task-driven (the extract_concepts worker), not HTTP-driven, in
// Phase 1.
type Concepts struct {
	deps Deps
}

func NewConcepts(deps Deps) *Concepts {
	return &Concepts{deps: deps}
}

// conceptIDFromURL parses the {conceptID} route param into a
// pgtype.UUID. Mirrors investigationIDFromURL. Used by the
// /concepts/{conceptID} route and by /concepts/{conceptID}/facts.
func conceptIDFromURL(r *http.Request) (pgtype.UUID, error) {
	raw := chi.URLParam(r, "conceptID")
	if raw == "" {
		return pgtype.UUID{}, errors.New("concept_id is required")
	}
	var id pgtype.UUID
	if err := id.Scan(raw); err != nil {
		return pgtype.UUID{}, errors.New("invalid concept_id")
	}
	return id, nil
}

// conceptIDFromOtherURL parses the {otherConceptID} route param into
// a pgtype.UUID, used by the relation-details endpoint's second
// concept. Mirrors conceptIDFromURL but reads a distinct route param.
func conceptIDFromOtherURL(r *http.Request) (pgtype.UUID, error) {
	raw := chi.URLParam(r, "otherConceptID")
	if raw == "" {
		return pgtype.UUID{}, errors.New("other concept_id is required")
	}
	var id pgtype.UUID
	if err := id.Scan(raw); err != nil {
		return pgtype.UUID{}, errors.New("invalid other concept_id")
	}
	return id, nil
}

// resolveConceptName fetches a concept by UUID, verifies it belongs
// to repoID (cross-repo isolation), and returns its canonical_name.
// On miss/cross-repo it writes a 404 and returns ok=false so the
// caller can early-return. Used by the relation-details handler to
// resolve both sides of a pair to their grouping keys.
func resolveConceptName(w http.ResponseWriter, r *http.Request, queries *store.Queries, repoID, conceptID pgtype.UUID) (string, bool) {
	concept, err := queries.GetConceptByID(r.Context(), conceptID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "concept not found")
			return "", false
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get concept")
		return "", false
	}
	if concept.RepositoryID != repoID {
		httputil.WriteError(w, http.StatusNotFound, "concept not found")
		return "", false
	}
	return concept.CanonicalName, true
}

// factIDFromURL parses the {factID} route param into a pgtype.UUID.
// Mirrors the helper in source.go but kept local to avoid a cross-
// package dependency for a single two-line function.
func factIDFromURL(r *http.Request) (pgtype.UUID, error) {
	raw := chi.URLParam(r, "factID")
	if raw == "" {
		return pgtype.UUID{}, errors.New("fact_id is required")
	}
	var id pgtype.UUID
	if err := id.Scan(raw); err != nil {
		return pgtype.UUID{}, errors.New("invalid fact_id")
	}
	return id, nil
}

// ListConcepts handles GET /{repoID}/concepts.
// Returns one entry per canonical-name group (concepts with the
// same lower(canonical_name) but different contexts collapse into
// one entry with a contexts array). Optional `?q=` filters by
// canonical_name substring (case-insensitive). Groups are ordered
// by total fact_count DESC, canonical_name ASC. Paginated by group.
// The list response omits per-context aliases (the detail endpoint
// returns those); each context entry still carries its fact_count.
func (c *Concepts) ListConcepts(w http.ResponseWriter, r *http.Request) {
	pool := appmw.PoolFromContext(r.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	repoID, err := repoIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	limit, offset := parsePaging(r)

	rows, err := queries.ListGroupedConceptsByRepo(r.Context(), store.ListGroupedConceptsByRepoParams{
		RepositoryID: repoID,
		Q:            q,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list concepts")
		return
	}

	total, err := queries.CountGroupedConceptsByRepo(r.Context(), store.CountGroupedConceptsByRepoParams{
		RepositoryID: repoID,
		Q:            q,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to count concepts")
		return
	}

	// Group per-context rows by lower(canonical_name). The list
	// endpoint omits per-context aliases (the detail endpoint
	// returns them), so we pass a nil aliases map.
	groupRows := make([]concepts.GroupRow, 0, len(rows))
	for _, r := range rows {
		groupRows = append(groupRows, concepts.FromListGroupedConceptsByRepoRow(r))
	}
	groups := concepts.BuildGroups(groupRows, nil)
	page := concepts.Paginate(groups, offset, limit)

	httputil.WriteJSON(w, http.StatusOK, pageEnvelope{
		Data:   page,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

// GetConcept handles GET /{repoID}/concepts/{conceptID}.
// The primary detail endpoint: resolves the concept UUID to its
// canonical_name, then returns the whole group (every per-context
// row sharing the lower(canonical_name)) with per-context aliases
// populated. 404 if the conceptID doesn't exist in this repo
// (cross-repo isolation: a conceptID from repo A is invisible from
// repo B because the ownership check is scoped by repository_id).
func (c *Concepts) GetConcept(w http.ResponseWriter, r *http.Request) {
	pool := appmw.PoolFromContext(r.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	repoID, err := repoIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	conceptID, err := conceptIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	concept, err := queries.GetConceptByID(r.Context(), conceptID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "concept not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get concept")
		return
	}
	if concept.RepositoryID != repoID {
		httputil.WriteError(w, http.StatusNotFound, "concept not found")
		return
	}

	// Resolve the concept's whole group by its canonical_name.
	rows, err := queries.ListConceptsByRepoName(r.Context(), store.ListConceptsByRepoNameParams{
		RepositoryID: repoID,
		CanonicalName: concept.CanonicalName,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get concept group")
		return
	}
	if len(rows) == 0 {
		// Shouldn't happen (the concept row exists), but defend.
		httputil.WriteError(w, http.StatusNotFound, "concept not found")
		return
	}

	group, err := buildGroupWithAliases(r.Context(), queries, rows, concepts.FromListConceptsByRepoNameRow)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to load concept aliases")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, group)
}

// buildGroupWithAliases is the shared tail of the concept detail
// handlers: it adapts the lookup rows to groupRow, loads every
// context's aliases in one round-trip, and builds the Group. The
// adapt fn lets it work with the name-lookup row type.
func buildGroupWithAliases[T any](
	ctx context.Context,
	queries *store.Queries,
	rows []T,
	adapt func(T) concepts.GroupRow,
) (concepts.Group, error) {
	groupRows := make([]concepts.GroupRow, 0, len(rows))
	ids := make([]pgtype.UUID, 0, len(rows))
	for _, r := range rows {
		gr := adapt(r)
		groupRows = append(groupRows, gr)
		ids = append(ids, gr.ID)
	}
	aliases, err := concepts.LoadAliasesByConceptID(ctx, queries, ids)
	if err != nil {
		return concepts.Group{}, err
	}
	groups := concepts.BuildGroups(groupRows, aliases)
	if len(groups) == 0 {
		return concepts.Group{}, nil
	}
	// The lookup is keyed on a single group (one canonical_name),
	// so BuildGroups returns exactly one entry.
	return groups[0], nil
}

// ListConceptFacts handles GET /{repoID}/concepts/{conceptID}/facts.
// The "query DNA → 200 facts" view, scoped to a single context's
// concept_id. Facts stay compartmentalized per context: this
// endpoint does NOT union facts across the concept's sibling
// contexts. Paginated; ordered by first_seen_at so the oldest link
// is first (stable across pages). A cross-repo conceptID is a 404.
//
// Searchable via the optional `q` query param (websearch_to_tsquery
// against facts.search_tsv, which covers facts.text); empty/absent
// q = no filter. Useful for large concepts whose facts span many
// pages. The response is a pageEnvelope: {data, total, limit, offset}.
func (c *Concepts) ListConceptFacts(w http.ResponseWriter, r *http.Request) {
	pool := appmw.PoolFromContext(r.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	repoID, err := repoIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	conceptID, err := conceptIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Verify the concept belongs to the repo so a cross-repo id is a
	// 404, not a silent listing of another repo's facts.
	concept, err := queries.GetConceptByID(r.Context(), conceptID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "concept not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get concept")
		return
	}
	if concept.RepositoryID != repoID {
		httputil.WriteError(w, http.StatusNotFound, "concept not found")
		return
	}

	limit, offset := parsePaging(r)
	search := strings.TrimSpace(r.URL.Query().Get("q"))

	facts, err := queries.ListFactsByConcept(r.Context(), store.ListFactsByConceptParams{
		ConceptID: conceptID,
		Column2:   search,
		Limit:     int32(limit),
		Offset:    int32(offset),
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list facts for concept")
		return
	}

	total, err := queries.CountFactsByConcept(r.Context(), store.CountFactsByConceptParams{
		ConceptID: conceptID,
		Column2:   search,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to count facts for concept")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, pageEnvelope{
		Data:   facts,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

// ListFactConcepts handles GET /{repoID}/facts/{factID}/concepts.
// The inverse view: which concepts a fact links to. Used by the
// fact detail page to show the concept tags attached to a fact.
// Returns per-context rows (one row per (fact, concept) link), not
// grouped — the fact detail page shows each context as a separate
// tag. The fact's ownership is verified via its fact_sources (the
// fact must belong to a source in the route's repo).
func (c *Concepts) ListFactConcepts(w http.ResponseWriter, r *http.Request) {
	pool := appmw.PoolFromContext(r.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	repoID, err := repoIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	factID, err := factIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Verify the fact belongs to the repo via its sources.
	factSources, err := queries.ListFactSourcesByFact(r.Context(), factID)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to verify fact ownership")
		return
	}
	if !factOwnedByRepo(factSources, repoID, queries, r) {
		httputil.WriteError(w, http.StatusNotFound, "fact not found")
		return
	}

	linkedConcepts, err := queries.ListConceptsByFact(r.Context(), factID)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list concepts for fact")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, pageEnvelope{
		Data:   linkedConcepts,
		Total:  int64(len(linkedConcepts)),
		Limit:  len(linkedConcepts),
		Offset: 0,
	})
}

// factOwnedByRepo reports whether any of the fact's sources belongs
// to the given repo. The check is done by loading each source's
// repository_id; a fact with no sources is treated as not owned
// (orphaned facts are a transient state during dedup merge).
func factOwnedByRepo(factSources []store.OktRepositoryFactSource, repoID pgtype.UUID, queries *store.Queries, r *http.Request) bool {
	if len(factSources) == 0 {
		return false
	}
	for _, fs := range factSources {
		src, err := queries.GetSourceByID(r.Context(), fs.SourceID)
		if err != nil {
			continue
		}
		if src.RepositoryID == repoID {
			return true
		}
	}
	return false
}

// conceptRelationRow is the JSON shape of one entry in the relations
// list. `slug` is the related concept's group key (suitable for
// building a link to its by-slug detail page); `canonical_name` is a
// display representative of the related group; `shared_fact_count`
// is the number of distinct facts linked to BOTH the queried concept
// and the related one (deduped per fact, not per source).
type conceptRelationRow struct {
	ConceptID       string `json:"concept_id"`
	CanonicalName   string `json:"canonical_name"`
	SharedFactCount int64  `json:"shared_fact_count"`
}

// ListConceptRelations handles GET
// /{repoID}/concepts/{conceptID}/relations. Returns a paginated
// list of concepts related to the concept group identified by the
// {conceptID}'s canonical_name, ordered by relation strength
// (shared_fact_count DESC) then by the related name ASC. A "relation"
// is the set of facts linked to both concept groups; shared_fact_count
// is the distinct count of those shared facts (a fact confirmed by N
// sources counts once — the "per name, not per source" requirement).
//
// Served from the okt_repository.concept_relations materialized view,
// which is refreshed concurrently by the refresh_concept_relations
// task (enqueued at the end of every extract_concepts batch and on a
// periodic schedule). Reads are a single index range scan, immune to
// parallel load. Default page size 10, max 200 (clamped by
// parsePaging); the UI shows the top 10 by default and pages for more.
//
// 404 when the queried conceptID has no concept in this repo (cross-
// repo isolation: a conceptID from repo A is invisible from repo B
// because the ownership check is scoped by repository_id). Auth-only
// (inherits the source:read semantics of the other concept endpoints).
func (c *Concepts) ListConceptRelations(w http.ResponseWriter, r *http.Request) {
	pool := appmw.PoolFromContext(r.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	repoID, err := repoIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	conceptID, err := conceptIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Resolve the conceptID to its concept row (404 if missing or
	// cross-repo) so we can key the matview query on its
	// canonical_name. This replaces the old slug existence check.
	concept, err := queries.GetConceptByID(r.Context(), conceptID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "concept not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get concept")
		return
	}
	if concept.RepositoryID != repoID {
		httputil.WriteError(w, http.StatusNotFound, "concept not found")
		return
	}

	limit, offset := parsePaging(r)

	rows, err := queries.ListConceptRelationsByConceptName(r.Context(), store.ListConceptRelationsByConceptNameParams{
		RepositoryID: repoID,
		Lower:        concept.CanonicalName,
		Limit:        int32(limit),
		Offset:       int32(offset),
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list concept relations")
		return
	}

	total, err := queries.CountConceptRelationsByConceptName(r.Context(), store.CountConceptRelationsByConceptNameParams{
		RepositoryID: repoID,
		Lower:        concept.CanonicalName,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to count concept relations")
		return
	}

	// Adapt the sqlc row (CanonicalName and ConceptID are interface{}
	// because MAX/MIN over text/uuid columns in a UNION subquery
	// defeat sqlc's type inference) into the clean conceptRelationRow
	// JSON shape. The underlying values from pgx are a string and a
	// pgtype.UUID respectively; the name falls back to other_name
	// (lower(canonical_name)) so the UI never renders a missing name.
	out := make([]conceptRelationRow, 0, len(rows))
	for _, row := range rows {
		name, _ := row.CanonicalName.(string)
		if name == "" {
			name = row.OtherName
		}
		out = append(out, conceptRelationRow{
			ConceptID:       pgUUIDInterfaceToString(row.ConceptID),
			CanonicalName:   name,
			SharedFactCount: row.SharedFactCount,
		})
	}

	httputil.WriteJSON(w, http.StatusOK, pageEnvelope{
		Data:   out,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

// conceptRelationDetailRow is the JSON shape of one entry in the
// relation-details response. `context` is the queried concept's
// context (one row per context of A); `shared_fact_count` is the
// number of distinct facts shared between that context's concept_id
// and ANY of the related concept's contexts (aggregated across all of
// B's contexts); `fact_ids` is the list of those shared fact ids so
// the UI can offer a "view shared facts" drill-down.
type conceptRelationDetailRow struct {
	Context         string   `json:"context"`
	SharedFactCount int64    `json:"shared_fact_count"`
	FactIDs         []string `json:"fact_ids"`
}

// GetConceptRelationDetails handles GET
// /{repoID}/concepts/{conceptID}/relations/{otherConceptID}. Returns
// the per-context breakdown of the relation between concept group
// `{conceptID}` and concept group `{otherConceptID}`: one row per
// CONTEXT of the queried concept (A), with shared_fact_count
// aggregated across all of the related concept's (B's) contexts,
// plus the list of shared fact_ids.
//
// Unlike ListConceptRelations (which reads the matview), this is a
// LIVE query against fact_concepts. Rationale: the details endpoint is
// on-demand for a specific pair (low volume) and benefits from
// freshness (a just-extracted shared fact shows up immediately without
// waiting for the matview refresh). The cost is bounded by A's fact
// count × concepts-per-fact and the pair filter keeps the working set
// small.
//
// 404 when either conceptID has no concept in this repo. Auth-only.
func (c *Concepts) GetConceptRelationDetails(w http.ResponseWriter, r *http.Request) {
	pool := appmw.PoolFromContext(r.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	repoID, err := repoIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	conceptID, err := conceptIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	otherConceptID, err := conceptIDFromOtherURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if conceptID == otherConceptID {
		httputil.WriteError(w, http.StatusBadRequest, "a concept is not related to itself")
		return
	}

	// Resolve both concept UUIDs to their concept rows (404 on either
	// if missing or cross-repo) so we can key the live pair query on
	// their canonical_names. This replaces the old per-slug existence
	// loop and keeps cross-repo isolation symmetric with the list
	// endpoint.
	nameA, ok := resolveConceptName(w, r, queries, repoID, conceptID)
	if !ok {
		return
	}
	nameB, ok := resolveConceptName(w, r, queries, repoID, otherConceptID)
	if !ok {
		return
	}

	rows, err := queries.ListConceptRelationDetailsByConceptName(r.Context(), store.ListConceptRelationDetailsByConceptNameParams{
		RepositoryID: repoID,
		Lower:       nameA,
		Lower_2:     nameB,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to load relation details")
		return
	}

	out := make([]conceptRelationDetailRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, conceptRelationDetailRow{
			Context:         row.Context,
			SharedFactCount: row.SharedFactCount,
			FactIDs:         pgUUIDArrayToStrings(row.FactIds),
		})
	}

	httputil.WriteJSON(w, http.StatusOK, pageEnvelope{
		Data:   out,
		Total:  int64(len(out)),
		Limit:  len(out),
		Offset: 0,
	})
}

// pgUUIDArrayToStrings coerces the value pgx returns for an
// ARRAY_AGG(uuid) column scanned into interface{} into a slice of
// canonical UUID strings. sqlc types ARRAY_AGG columns as interface{}
// because the element type isn't statically known to it; pgx may
// deliver the value as []pgtype.UUID, []string, or [][]byte depending
// on the connection's type registry. Each shape is handled; anything
// unexpected falls back to an empty slice so the JSON never carries a
// malformed value.
func pgUUIDArrayToStrings(v interface{}) []string {
	if v == nil {
		return nil
	}
	switch arr := v.(type) {
	case []pgtype.UUID:
		out := make([]string, 0, len(arr))
		for _, id := range arr {
			if id.Valid {
				out = append(out, uuidFromPgtype(id))
			}
		}
		return out
	case []string:
		out := make([]string, 0, len(arr))
		for _, s := range arr {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case [][]byte:
		out := make([]string, 0, len(arr))
		for _, b := range arr {
			if len(b) > 0 {
				out = append(out, string(b))
			}
		}
		return out
	default:
		return nil
	}
}

// pgUUIDInterfaceToString coerces the value pgx returns for a
// MIN(uuid) aggregate column (typed interface{} by sqlc because the
// UNION subquery defeats type inference) into the canonical
// lowercase 36-char string. pgx may deliver the value as a
// pgtype.UUID, a string, or []byte; anything unexpected falls back
// to "" so the JSON concept_id is absent rather than malformed.
func pgUUIDInterfaceToString(v interface{}) string {
	switch id := v.(type) {
	case pgtype.UUID:
		if id.Valid {
			return uuidFromPgtype(id)
		}
	case string:
		return id
	case []byte:
		return string(id)
	}
	return ""
}

var _ = uuid.New