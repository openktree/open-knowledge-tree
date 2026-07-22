// Package search implements the hybrid search ranking that fuses
// the lexical (Postgres tsvector) and semantic (Qdrant cosine)
// search channels via Reciprocal Rank Fusion (RRF). The package is
// transport-agnostic: it knows about the store and qdrantstore
// layers but nothing about HTTP, so it can be reused by a CLI or
// worker if a future phase needs hybrid ranking outside the
// request path.
package search

import (
	"sort"

	"github.com/google/uuid"
)

// IDRank is one ranked hit from a single channel (lexical or
// semantic). ID is the fact or concept UUID; Rank is the 0-based
// position in that channel's result list (the top hit is rank 0);
// Score is the channel-specific score (ts_rank_cd for lexical,
// cosine similarity for semantic) carried for debugging/UI.
type IDRank struct {
	ID    uuid.UUID
	Rank  int
	Score float64
}

// RankedID is a fused hit: the UUID with the combined RRF score
// and the per-channel ranks that produced it (LexicalRank/SemanticRank
// are -1 when the channel did not return the id). The fused score
// is the sum of the per-channel RRF contributions, so an id present
// in both channels scores higher than one present in only one.
type RankedID struct {
	ID            uuid.UUID
	FusedScore    float64
	LexicalRank   int
	SemanticRank  int
	LexicalScore  float64
	SemanticScore float64
}

// RRF fuses two ranked id lists with Reciprocal Rank Fusion. The
// standard damping constant k (typically 60) controls how much the
// top ranks dominate: each channel contributes 1/(k + rank) to the
// fused score, so rank 0 contributes 1/k, rank 1 contributes
// 1/(k+1), etc. An id present in both channels gets the sum; an id
// in only one channel gets only that channel's contribution.
//
// The result is sorted by FusedScore DESC, then by LexicalRank ASC
// (so when scores tie the lexical top hits stay on top — a stable,
// deterministic order that mirrors the pre-hybrid behavior), then
// by SemanticRank ASC.
//
// k must be > 0; passing k <= 0 falls back to 60 (the standard
// default) so a misconfigured deployment still gets a sane fusion.
func RRF(lexical, semantic []IDRank, k int) []RankedID {
	if k <= 0 {
		k = 60
	}
	merged := make(map[uuid.UUID]*RankedID, len(lexical)+len(semantic))
	contribute := func(ranks []IDRank, setRank func(*RankedID, int, float64)) {
		for _, r := range ranks {
			entry, ok := merged[r.ID]
			if !ok {
				entry = &RankedID{
					ID:          r.ID,
					LexicalRank: -1,
					SemanticRank: -1,
				}
				merged[r.ID] = entry
			}
			entry.FusedScore += 1.0 / float64(k+r.Rank)
			setRank(entry, r.Rank, r.Score)
		}
	}
	contribute(lexical, func(e *RankedID, rank int, score float64) {
		e.LexicalRank = rank
		e.LexicalScore = score
	})
	contribute(semantic, func(e *RankedID, rank int, score float64) {
		e.SemanticRank = rank
		e.SemanticScore = score
	})

	out := make([]RankedID, 0, len(merged))
	for _, e := range merged {
		out = append(out, *e)
	}
	sortRankedIDs(out)
	return out
}

// sortRankedIDs sorts in place by FusedScore DESC, then by
// LexicalRank ASC (treating -1 as +inf so ids absent from the
// lexical channel sink), then by SemanticRank ASC. This keeps the
// fused order stable and deterministic across runs.
func sortRankedIDs(out []RankedID) {
	lexRank := func(r RankedID) int {
		if r.LexicalRank < 0 {
			return 1 << 30
		}
		return r.LexicalRank
	}
	semRank := func(r RankedID) int {
		if r.SemanticRank < 0 {
			return 1 << 30
		}
		return r.SemanticRank
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].FusedScore != out[j].FusedScore {
			return out[i].FusedScore > out[j].FusedScore
		}
		if lexRank(out[i]) != lexRank(out[j]) {
			return lexRank(out[i]) < lexRank(out[j])
		}
		return semRank(out[i]) < semRank(out[j])
	})
}

// TakeFusedIDs returns the UUIDs of the first n fused entries, in
// fused order. Used by the hybrid service to build the ordered id
// list that drives the final Postgres fetch: the caller fetches the
// rows for those ids and re-orders them by the fused order in Go.
func TakeFusedIDs(ranked []RankedID, n int) []uuid.UUID {
	if n <= 0 || len(ranked) == 0 {
		return nil
	}
	if n > len(ranked) {
		n = len(ranked)
	}
	ids := make([]uuid.UUID, n)
	for i := 0; i < n; i++ {
		ids[i] = ranked[i].ID
	}
	return ids
}