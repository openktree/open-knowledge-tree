package search

import (
	"testing"

	"github.com/google/uuid"
)

func TestRRF_IDPresentInBothChannelsScoresHigher(t *testing.T) {
	lexOnly := uuid.New()
	semOnly := uuid.New()
	both := uuid.New()

	lexical := []IDRank{
		{ID: lexOnly, Rank: 0, Score: 0.9},
		{ID: both, Rank: 1, Score: 0.7},
	}
	semantic := []IDRank{
		{ID: both, Rank: 0, Score: 0.95},
		{ID: semOnly, Rank: 1, Score: 0.6},
	}

	got := RRF(lexical, semantic, 60)

	// `both` is rank 1 in lexical (1/(60+1)) and rank 0 in semantic
	// (1/60), so it should beat `lexOnly` (only 1/60 from lexical)
	// and `semOnly` (only 1/61 from semantic).
	if got[0].ID != both {
		t.Fatalf("expected `both` first, got %s (score %v)", got[0].ID, got[0].FusedScore)
	}
	// Lexical-only then semantic-only. Their exact scores depend
	// on k=60 but the relative order is lexOnly (1/60) > semOnly
	// (1/61) because rank 0 in lexical beats rank 1 in semantic.
	if got[1].ID != lexOnly {
		t.Fatalf("expected `lexOnly` second, got %s", got[1].ID)
	}
	if got[2].ID != semOnly {
		t.Fatalf("expected `semOnly` third, got %s", got[2].ID)
	}

	// Per-channel ranks/scores are preserved on the merged entry.
	bothEntry := got[0]
	if bothEntry.LexicalRank != 1 || bothEntry.LexicalScore != 0.7 {
		t.Errorf("lexical rank/score not preserved: %+v", bothEntry)
	}
	if bothEntry.SemanticRank != 0 || bothEntry.SemanticScore != 0.95 {
		t.Errorf("semantic rank/score not preserved: %+v", bothEntry)
	}
}

func TestRRF_TieBreakPrefersLexicalTopHit(t *testing.T) {
	// Two ids both appearing only in lexical at rank 0 and 1.
	// FusedScore differs (1/60 vs 1/61) so no tie; just verify
	// the lexical top hit is first.
	a := uuid.New()
	b := uuid.New()
	got := RRF([]IDRank{{ID: a, Rank: 0}, {ID: b, Rank: 1}}, nil, 60)
	if len(got) != 2 || got[0].ID != a || got[1].ID != b {
		t.Fatalf("lexical order not preserved: %+v", got)
	}
}

func TestRRF_EmptyInputsReturnsEmpty(t *testing.T) {
	if got := RRF(nil, nil, 60); len(got) != 0 {
		t.Fatalf("expected empty, got %+v", got)
	}
	if got := RRF(nil, nil, 0); len(got) != 0 {
		t.Fatalf("k<=0 fallback should still return empty, got %+v", got)
	}
}

func TestRRF_KDefaultsTo60(t *testing.T) {
	// With k=0 (misconfigured) we fall back to 60, so a single
	// rank-0 hit scores 1/60.
	got := RRF([]IDRank{{ID: uuid.New(), Rank: 0}}, nil, 0)
	if got[0].FusedScore != 1.0/60.0 {
		t.Fatalf("expected fallback k=60 -> 1/60, got %v", got[0].FusedScore)
	}
}

func TestRRF_AbsentChannelRanksAreMinusOne(t *testing.T) {
	id := uuid.New()
	got := RRF([]IDRank{{ID: id, Rank: 2, Score: 0.5}}, nil, 60)
	if got[0].LexicalRank != 2 {
		t.Errorf("lexical rank should be 2, got %d", got[0].LexicalRank)
	}
	if got[0].SemanticRank != -1 {
		t.Errorf("semantic rank should be -1 (absent), got %d", got[0].SemanticRank)
	}
}

func TestTakeFusedIDs_LimitsToN(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	ranked := []RankedID{
		{ID: a, FusedScore: 0.5},
		{ID: b, FusedScore: 0.3},
		{ID: c, FusedScore: 0.1},
	}
	if got := TakeFusedIDs(ranked, 2); len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("expected [a, b], got %v", got)
	}
	if got := TakeFusedIDs(ranked, 0); len(got) != 0 {
		t.Fatalf("n=0 should return empty, got %v", got)
	}
	if got := TakeFusedIDs(ranked, 99); len(got) != 3 {
		t.Fatalf("n > len should clamp to len, got %d", len(got))
	}
}