package tasks

import (
	"testing"

	"github.com/openktree/open-knowledge-tree/backend/internal/config"
)

func TestEmbModelMismatch(t *testing.T) {
	cases := []struct {
		name     string
		stats    ImportStats
		local    string
		localDim int
		want     bool
	}{
		{
			name:  "exact match bare model",
			stats: ImportStats{ImportedEmbModels: []string{"gemini-embedding-2"}, ImportedEmbDims: []int{3072}},
			local: "gemini-embedding-2", localDim: 3072,
			want: false,
		},
		{
			name:  "match after normalization (OpenRouter prefix + tag)",
			stats: ImportStats{ImportedEmbModels: []string{"google/gemini-embedding-2:free"}, ImportedEmbDims: []int{3072}},
			local: "google/gemini-embedding-2", localDim: 3072,
			want: false,
		},
		{
			name:  "cross-provider same model (OpenRouter vs Ollama)",
			stats: ImportStats{ImportedEmbModels: []string{"google/gemini-embedding-2"}, ImportedEmbDims: []int{3072}},
			local: "gemini-embedding-2", localDim: 3072,
			want: false,
		},
		{
			name:  "different model",
			stats: ImportStats{ImportedEmbModels: []string{"text-embedding-3-large"}, ImportedEmbDims: []int{3072}},
			local: "gemini-embedding-2", localDim: 3072,
			want: true,
		},
		{
			name:  "empty imported model is ignored",
			stats: ImportStats{ImportedEmbModels: []string{""}, ImportedEmbDims: []int{0}},
			local: "gemini-embedding-2", localDim: 3072,
			want: false,
		},
		{
			name:  "dimension mismatch only (same model, different dims)",
			stats: ImportStats{ImportedEmbModels: []string{"gemini-embedding-2"}, ImportedEmbDims: []int{1024}},
			local: "gemini-embedding-2", localDim: 3072,
			want: true,
		},
		{
			name:  "imported dims unknown (0) does not trigger mismatch",
			stats: ImportStats{ImportedEmbModels: []string{"gemini-embedding-2"}, ImportedEmbDims: []int{0}},
			local: "gemini-embedding-2", localDim: 3072,
			want: false,
		},
		{
			name:  "local dims unknown (0) does not trigger mismatch",
			stats: ImportStats{ImportedEmbModels: []string{"gemini-embedding-2"}, ImportedEmbDims: []int{3072}},
			local: "gemini-embedding-2", localDim: 0,
			want: false,
		},
		{
			name:  "mixed: one matches one mismatches",
			stats: ImportStats{ImportedEmbModels: []string{"gemini-embedding-2", "text-embedding-3-large"}, ImportedEmbDims: []int{3072, 3072}},
			local: "gemini-embedding-2", localDim: 3072,
			want: true,
		},
		{
			name:  "mixed: one matches one has dim mismatch",
			stats: ImportStats{ImportedEmbModels: []string{"gemini-embedding-2", "gemini-embedding-2"}, ImportedEmbDims: []int{3072, 1024}},
			local: "gemini-embedding-2", localDim: 3072,
			want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.stats.EmbModelMismatch(c.local, c.localDim)
			if got != c.want {
				t.Errorf("EmbModelMismatch(%q, dim=%d) = %v, want %v\nstats: %+v", c.local, c.localDim, got, c.want, c.stats)
			}
		})
	}
}

func TestPlan(t *testing.T) {
	reconciler := NewCacheReconciler(config.EmbeddingConfig{Model: "gemini-embedding-2", Dimensions: 3072})

	cases := []struct {
		name  string
		stats ImportStats
		want  ReconcilePlan
	}{
		{
			name:  "no delta (created=0) → empty plan",
			stats: ImportStats{Created: 0, Skipped: 5},
			want:  ReconcilePlan{},
		},
		{
			name:  "delta + matching model → DedupFacts",
			stats: ImportStats{Created: 3, ImportedEmbModels: []string{"google/gemini-embedding-2:free"}, ImportedEmbDims: []int{3072}},
			want:  ReconcilePlan{DedupFacts: true},
		},
		{
			name:  "delta + model mismatch → ReembedFacts",
			stats: ImportStats{Created: 3, ImportedEmbModels: []string{"text-embedding-3-large"}, ImportedEmbDims: []int{3072}},
			want:  ReconcilePlan{ReembedFacts: true},
		},
		{
			name:  "delta + dim mismatch → ReembedFacts",
			stats: ImportStats{Created: 3, ImportedEmbModels: []string{"gemini-embedding-2"}, ImportedEmbDims: []int{1024}},
			want:  ReconcilePlan{ReembedFacts: true},
		},
		{
			name:  "delta + no embedding info → DedupFacts (treat as match)",
			stats: ImportStats{Created: 3},
			want:  ReconcilePlan{DedupFacts: true},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := reconciler.Plan(c.stats)
			if got != c.want {
				t.Errorf("Plan() = %+v, want %+v\nstats: %+v", got, c.want, c.stats)
			}
		})
	}
}

func TestReconcilePlan_IsEmpty(t *testing.T) {
	if !(ReconcilePlan{}).IsEmpty() {
		t.Error("zero-value plan should be empty")
	}
	if (ReconcilePlan{ReembedFacts: true}).IsEmpty() {
		t.Error("ReembedFacts plan should not be empty")
	}
	if (ReconcilePlan{DedupFacts: true}).IsEmpty() {
		t.Error("DedupFacts plan should not be empty")
	}
}