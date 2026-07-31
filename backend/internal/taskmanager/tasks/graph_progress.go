package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// GraphProgress is the phase-level progress object the export_graph
// and import_graph workers write to river_job.metadata so the
// frontend can render a live progress stepper. River v0.39.0 has no
// native JobProgress API (MetadataSet defers the write to
// end-of-attempt, not mid-flight), so the workers UPDATE
// river_job.metadata directly via the task DB pool. The generic
// GET /tasks/{jobID} endpoint already returns metadata to the
// frontend, which renders it via the ExportProgress component.
//
// Phases (export): building → pushing → completed/failed.
// Phases (import): downloading → importing → completed/failed.
//
// BytesTransferred / TotalBytes are the realtime byte counters that
// tick during the long phases. During "building" the temp file grows
// as it's written, so TotalBytes is unknown (0) and the UI shows
// "12.3 MB written". During "pushing"/"downloading"/"importing" the
// total is known (file stat / Content-Length) and the UI shows
// "12.3 MB of 45.6 MB (27%)".
type GraphProgress struct {
	Phase        string    `json:"phase"`
	StartedAt    time.Time `json:"started_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	SourceCount  int       `json:"source_count,omitempty"`
	FactCount    int       `json:"fact_count,omitempty"`
	ConceptCount int       `json:"concept_count,omitempty"`
	BundleBytes  int64     `json:"bundle_bytes,omitempty"`
	GraphID      string    `json:"graph_id,omitempty"`
	Error        string    `json:"error,omitempty"`

	// BytesTransferred is the live byte count during a transfer
	// phase (building = bytes written to temp file; pushing =
	// bytes read from temp file and sent to registry; downloading
	// = bytes received from registry; importing = bytes read from
	// the temp file into the stream importer). Updated every ~2s
	// by the counting io wrappers via the throttled progressWriter.
	BytesTransferred int64 `json:"bytes_transferred,omitempty"`

	// TotalBytes is the total size of the transfer when known.
	// During "building" it is 0 (the file grows as it's written).
	// During "pushing"/"downloading"/"importing" it is the temp
	// file size or the HTTP Content-Length. When non-zero, the UI
	// renders a percentage fill bar.
	TotalBytes int64 `json:"total_bytes,omitempty"`
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

// throttledProgressWriter wraps updateGraphProgress with a
// time-based throttle so the counting io wrappers can call back
// frequently (per Write/Read chunk) without hammering the DB. The
// frontend polls at 3-10s, so writing faster than ~2s is wasted.
//
// The terminal phases ("completed", "failed") bypass the throttle
// so the UI gets the final state immediately.
type throttledProgressWriter struct {
	pool        *pgxpool.Pool
	jobID       int64
	lastWrite   time.Time
	minInterval time.Duration
	ctx         context.Context
}

// newThrottledProgressWriter builds a writer with a 2s default
// throttle. The ctx is captured for the update calls so the
// counting wrappers don't need to thread it through Read/Write.
func newThrottledProgressWriter(ctx context.Context, pool *pgxpool.Pool, jobID int64) *throttledProgressWriter {
	return &throttledProgressWriter{
		pool:        pool,
		jobID:       jobID,
		minInterval: 2 * time.Second,
		ctx:         ctx,
	}
}

// update writes progress to river_job.metadata unless the call is
// throttled (< minInterval since last write) AND the phase is not
// terminal. Terminal phases always flush.
func (pw *throttledProgressWriter) update(p GraphProgress) {
	if pw.pool == nil {
		return
	}
	isTerminal := p.Phase == "completed" || p.Phase == "failed"
	if !isTerminal && time.Since(pw.lastWrite) < pw.minInterval {
		return
	}
	pw.lastWrite = time.Now()
	updateGraphProgress(pw.ctx, pw.pool, pw.jobID, p)
}

// countingWriter is an io.Writer that tracks bytes written and
// calls cb periodically (every ~2s or on the final write). Used to
// report live "bytes written" progress during the export building
// phase (StreamBuild writes to a temp file through this wrapper).
type countingWriter struct {
	w       io.Writer
	written int64
	total   int64 // 0 = unknown (file grows as it's written)
	cb      func(bytesTransferred, total int64)
	lastCB  time.Time
}

// newCountingWriter wraps w with a byte counter. cb is called at
// most every ~2s (and on the final flush). total is the expected
// size when known (0 = unknown/growing).
func newCountingWriter(w io.Writer, total int64, cb func(bytesTransferred, total int64)) *countingWriter {
	return &countingWriter{w: w, total: total, cb: cb}
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.written += int64(n)
	if cw.cb != nil && (time.Since(cw.lastCB) > 2*time.Second || (cw.total > 0 && cw.written >= cw.total)) {
		cw.lastCB = time.Now()
		cw.cb(cw.written, cw.total)
	}
	return n, err
}

// flush forces a final callback with the total written. Called
// after the last Write so the UI sees the complete byte count.
func (cw *countingWriter) flush() {
	if cw.cb != nil {
		cw.cb(cw.written, cw.total)
	}
}

// countingReader is an io.Reader that tracks bytes read and calls
// cb periodically. Used to report live "bytes uploaded/downloaded"
// progress during the export pushing phase (reading the temp file
// to send to the registry) and the import downloading/importing
// phases (reading the downloaded bundle into the stream importer).
type countingReader struct {
	r       io.Reader
	read    int64
	total   int64 // 0 = unknown
	cb      func(bytesTransferred, total int64)
	lastCB  time.Time
}

// newCountingReader wraps r with a byte counter. cb is called at
// most every ~2s (and on EOF / when total is reached). total is
// the expected size when known (Content-Length / file stat).
func newCountingReader(r io.Reader, total int64, cb func(bytesTransferred, total int64)) *countingReader {
	return &countingReader{r: r, total: total, cb: cb}
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.read += int64(n)
	if cr.cb != nil && n > 0 && (time.Since(cr.lastCB) > 2*time.Second || (cr.total > 0 && cr.read >= cr.total)) {
		cr.lastCB = time.Now()
		cr.cb(cr.read, cr.total)
	}
	return n, err
}

// flush forces a final callback with the total read.
func (cr *countingReader) flush() {
	if cr.cb != nil {
		cr.cb(cr.read, cr.total)
	}
}

// reportCountingError is a best-effort log so a counting-wrapper
// failure (e.g. the callback panicked) doesn't kill the worker.
// The wrappers themselves never return errors from cb — the
// caller's Read/Write error is what matters.
func reportCountingError(err any) {
	log.Printf("graph progress: counting wrapper callback panic: %v", err)
}

var _ = fmt.Sprintf