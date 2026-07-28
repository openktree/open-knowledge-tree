package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// GraphProgress is the phase-level progress object the export_graph
// and import_graph workers write to river_job.metadata so the
// frontend can render a live progress stepper. River v0.39.0 has no
// native JobProgress API (the metadata column is the only channel),
// so the workers UPDATE river_job.metadata directly via the task DB
// pool at each phase boundary. The generic GET /tasks/{jobID}
// endpoint already returns metadata to the frontend, which renders
// it via the ExportProgress component.
//
// Phases (export): pass1_hashing → pass2_writing → pushing →
// completed/failed.
// Phases (import): downloading → decoding → importing →
// completed/failed.
type GraphProgress struct {
	Phase       string    `json:"phase"`
	StartedAt   time.Time `json:"started_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	SourceCount int       `json:"source_count,omitempty"`
	FactCount   int       `json:"fact_count,omitempty"`
	ConceptCount int      `json:"concept_count,omitempty"`
	BundleBytes  int64    `json:"bundle_bytes,omitempty"`
	GraphID     string    `json:"graph_id,omitempty"`
	Error       string    `json:"error,omitempty"`
}

// updateGraphProgress merges a GraphProgress object into the
// river_job.metadata jsonb column for the given job ID. The merge
// is a metadata || jsonb build so existing keys (like repo_id) are
// preserved. Best-effort: errors are logged but don't fail the job
// (progress is a UX nicety, not a correctness requirement).
func updateGraphProgress(ctx context.Context, pool *pgxpool.Pool, jobID int64, progress GraphProgress) {
	progress.UpdatedAt = time.Now().UTC()
	progressJSON, err := json.Marshal(map[string]any{"progress": progress})
	if err != nil {
		return
	}
	_, _ = pool.Exec(ctx,
		`UPDATE okt_system.river_job SET metadata = metadata || $2::jsonb WHERE id = $1`,
		jobID, progressJSON,
	)
}

// progressPhaseError wraps an error with a progress update marking
// the phase as failed. Used in defer or before returning an error
// from the worker's Work method so the UI shows the failure reason.
func progressPhaseError(ctx context.Context, pool *pgxpool.Pool, jobID int64, phase, errMsg string) {
	updateGraphProgress(ctx, pool, jobID, GraphProgress{
		Phase: phase,
		Error: errMsg,
	})
}

var _ = fmt.Sprintf