// Package concepts holds the domain logic for concept grouping:
// collapsing per-context concept rows into one group per canonical
// name so the API can present "one concept, many contexts". The
// package is transport-agnostic — it knows about the store layer
// but nothing about HTTP — so it can be reused by a CLI or worker
// if a future phase needs grouping outside the request path.
package concepts

import (
	"context"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// ContextEntry is one context slice of a concept group: the
// per-context concept row (id, context, fact_count, etc.) plus the
// aliases attached to that specific context's concept_id. Facts
// stay linked to the concept_id, so the "facts for this context"
// view is still keyed on ContextEntry.ConceptID.
type ContextEntry struct {
	ConceptID     pgtype.UUID        `json:"concept_id"`
	Context       string             `json:"context"`
	FactCount     int64              `json:"fact_count"`
	Description   *string            `json:"description"`
	EmbeddedAt    pgtype.Timestamptz `json:"embedded_at"`
	EmbeddedModel *string            `json:"embedded_model"`
	CreatedAt     pgtype.Timestamptz `json:"created_at"`
	Aliases       []string           `json:"aliases"`
}

// Group is the unified view of a concept: one canonical name with
// its total fact count summed across every context, plus the list
// of per-context entries. The list endpoint returns []Group; the
// detail endpoint returns a single Group (with the contexts'
// aliases populated).
//
// SearchRank is the relevance rank of the group with respect to the
// caller's @q filter (0.0 when @q is empty). It is the MAX of the
// per-context search ranks returned by the SQL query, so a hit on
// any context ranks the whole group. BuildGroups sorts by
// SearchRank DESC first (so name/alias hits rank above description-
// only hits, since name/alias use tsv weight A and description uses
// weight D), then by TotalFactCount DESC, CanonicalName ASC.
type Group struct {
	CanonicalName  string         `json:"canonical_name"`
	TotalFactCount int64          `json:"total_fact_count"`
	SearchRank     float32        `json:"search_rank"`
	Contexts       []ContextEntry `json:"contexts"`
}

// BuildGroups groups per-context concept rows by lower(canonical_name)
// into a sorted []Group. The input rows must already be ordered by
// fact_count DESC, canonical_name ASC (the SQL query enforces this)
// so the first row seen for a name is the highest-fact_count context
// — it becomes the group's display CanonicalName and the first
// ContextEntry. Groups are sorted by SearchRank DESC, then
// TotalFactCount DESC, CanonicalName ASC. The SearchRank tie-break
// only matters when @q is non-empty (rows carry a non-zero
// name_rank/alias_rank); with an empty @q every rank is 0.0 and the
// sort degenerates to the pre-existing fact_count / name order.
//
// aliasesByID maps concept_id -> alias texts; it's used to populate
// each ContextEntry.Aliases. Pass nil for the list endpoint (the
// list doesn't show per-context aliases, only counts); pass the
// real map for the detail endpoint.
func BuildGroups(rows []groupRow, aliasesByID map[pgtype.UUID][]string) []Group {
	if len(rows) == 0 {
		return nil
	}
	// Preserve first-seen order for the group representative while
	// we accumulate; we'll sort the final slice at the end.
	order := make([]string, 0, len(rows))
	byKey := make(map[string]*Group, len(rows))

	for _, r := range rows {
		key := strings.ToLower(r.CanonicalName)
		g, ok := byKey[key]
		if !ok {
			g = &Group{
				CanonicalName: r.CanonicalName,
			}
			byKey[key] = g
			order = append(order, key)
		}
		entry := ContextEntry{
			ConceptID:     r.ID,
			Context:       r.Context,
			FactCount:     r.FactCount,
			Description:   r.Description,
			EmbeddedAt:    r.EmbeddedAt,
			EmbeddedModel: r.EmbeddedModel,
			CreatedAt:     r.CreatedAt,
		}
		if aliasesByID != nil {
			entry.Aliases = aliasesByID[r.ID]
		}
		g.TotalFactCount += r.FactCount
		// search_rank for the group is the MAX across contexts: a
		// hit on any context's concept (name/description/alias)
		// ranks the whole group at that hit's relevance.
		if rank := r.searchRank(); rank > g.SearchRank {
			g.SearchRank = rank
		}
		g.Contexts = append(g.Contexts, entry)
	}

	groups := make([]Group, 0, len(order))
	for _, k := range order {
		groups = append(groups, *byKey[k])
	}
	sortGroups(groups)
	return groups
}

// sortGroups sorts in place by SearchRank DESC, then TotalFactCount
// DESC, CanonicalName ASC. SearchRank is 0.0 for an unfiltered
// listing, so the rank tie-break is a no-op there and the order
// falls back to fact_count / name (the pre-existing behavior).
func sortGroups(groups []Group) {
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].SearchRank != groups[j].SearchRank {
			return groups[i].SearchRank > groups[j].SearchRank
		}
		if groups[i].TotalFactCount != groups[j].TotalFactCount {
			return groups[i].TotalFactCount > groups[j].TotalFactCount
		}
		return groups[i].CanonicalName < groups[j].CanonicalName
	})
}

// groupRow is the minimal row shape BuildGroups needs. The store's
// generated ListGroupedConceptsByRepoRow / ListConceptsByRepoNameRow
// / ListGroupedInvestigationConceptsRow all satisfy it via the
// adapter funcs below. It's exported as GroupRow so handler code can
// build a slice of them and read the ID field for the alias-batch
// query without depending on the concrete store row types.
//
// nameRank / aliasRank are only populated by the search query
// (ListGroupedConceptsByRepo); the name-lookup and investigation
// queries leave them zero. searchRank combines them as the MAX of
// the two so a concept's own name/description hit and an alias hit
// are interchangeable for ranking.
type groupRow struct {
	ID            pgtype.UUID
	CanonicalName string
	Context       string
	Description   *string
	EmbeddedAt    pgtype.Timestamptz
	EmbeddedModel *string
	CreatedAt     pgtype.Timestamptz
	FactCount     int64
	NameRank      float32
	AliasRank     float32
}

// searchRank returns the per-row relevance used by BuildGroups to
// order groups: the higher of the concept's own name/description
// rank and its best alias rank. Both are 0.0 for unfiltered rows.
func (r groupRow) searchRank() float32 {
	if r.NameRank > r.AliasRank {
		return r.NameRank
	}
	return r.AliasRank
}

// GroupRow is the exported alias for groupRow so handler code can
// accumulate adapted rows in a slice and read exported fields. The
// adapter funcs return groupRow values, which fit this alias.
type GroupRow = groupRow

// FromListGroupedConceptsByRepoRow adapts the generated
// repo-scoped list row to groupRow. This is the only adapter that
// carries search rank (name_rank + alias_rank from the weighted
// websearch_to_tsquery in ListGroupedConceptsByRepo); the
// name-lookup and investigation adapters leave rank zero.
func FromListGroupedConceptsByRepoRow(r store.ListGroupedConceptsByRepoRow) groupRow {
	return groupRow{
		ID:            r.ID,
		CanonicalName: r.CanonicalName,
		Context:       r.Context,
		Description:   r.Description,
		EmbeddedAt:    r.EmbeddedAt,
		EmbeddedModel: r.EmbeddedModel,
		CreatedAt:     r.CreatedAt,
		FactCount:     r.FactCount,
		NameRank:      r.NameRank,
		AliasRank:     r.AliasRank,
	}
}

// FromListConceptsByRepoNameRow adapts the generated name-lookup
// row to groupRow.
func FromListConceptsByRepoNameRow(r store.ListConceptsByRepoNameRow) groupRow {
	return groupRow{
		ID:            r.ID,
		CanonicalName: r.CanonicalName,
		Context:       r.Context,
		Description:   r.Description,
		EmbeddedAt:    r.EmbeddedAt,
		EmbeddedModel: r.EmbeddedModel,
		CreatedAt:     r.CreatedAt,
		FactCount:     r.FactCount,
	}
}

// FromListGroupedInvestigationConceptsRow adapts the generated
// investigation-scoped list row to groupRow.
func FromListGroupedInvestigationConceptsRow(r store.ListGroupedInvestigationConceptsRow) groupRow {
	return groupRow{
		ID:            r.ID,
		CanonicalName: r.CanonicalName,
		Context:       r.Context,
		Description:   r.Description,
		EmbeddedAt:    r.EmbeddedAt,
		EmbeddedModel: r.EmbeddedModel,
		CreatedAt:     r.CreatedAt,
		FactCount:     r.FactCount,
	}
}

// LoadAliasesByConceptID fetches aliases for the given concept_ids
// in one query and returns a map keyed by concept_id. Used by the
// detail endpoint to populate every context's aliases in a single
// round-trip instead of N per-context calls.
func LoadAliasesByConceptID(ctx context.Context, queries *store.Queries, ids []pgtype.UUID) (map[pgtype.UUID][]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := queries.ListConceptAliasesByConceptIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	m := make(map[pgtype.UUID][]string, len(ids))
	for _, a := range rows {
		m[a.ConceptID] = append(m[a.ConceptID], a.AliasText)
	}
	return m, nil
}

// Paginate slices groups to [offset, offset+limit) with bounds
// clamping. Used by the list handler after BuildGroups.
func Paginate(groups []Group, offset, limit int) []Group {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(groups) {
		return nil
	}
	end := offset + limit
	if end > len(groups) {
		end = len(groups)
	}
	if limit <= 0 {
		return nil
	}
	return groups[offset:end]
}