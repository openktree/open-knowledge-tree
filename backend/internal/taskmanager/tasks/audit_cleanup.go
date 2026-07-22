package tasks

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/riverqueue/river"
)

const QueueAuditCleanup = "audit_cleanup"

// AuditCleanupArgs is the (empty) argument shape for the daily
// audit_cleanup periodic job. The cutoff is derived from
// cfg.Audit.RetentionDays at work time so a config reload (no
// restart) takes effect on the next run.
type AuditCleanupArgs struct{}

func (AuditCleanupArgs) Kind() string { return "audit_cleanup" }

func (AuditCleanupArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueAuditCleanup}
}

type AuditCleanupResult struct {
	Deleted int64 `json:"deleted"`
}

type AuditCleanupWorker struct {
	river.WorkerDefaults[AuditCleanupArgs]
	queries *store.Queries
	cfg     config.AuditConfig
}

func NewAuditCleanupWorker(queries *store.Queries, cfg config.AuditConfig) *AuditCleanupWorker {
	return &AuditCleanupWorker{queries: queries, cfg: cfg}
}

// Work deletes okt_system.permission_audit rows older than
// cfg.Audit.RetentionDays (default 30). Runs once a day; the
// periodic-job constructor in taskmanager.New pins the cadence.
// RunOnStart is false so a boot doesn't trigger an immediate
// sweep — the first run is 24h after the client starts. The
// deletion is a single statement; the table is indexed on
// occurred_at DESC so the planner can use the index to find the
// cutoff boundary cheaply.
func (w *AuditCleanupWorker) Work(ctx context.Context, job *river.Job[AuditCleanupArgs]) error {
	if w.queries == nil {
		return river.RecordOutput(ctx, &AuditCleanupResult{})
	}
	days := w.cfg.RetentionDaysOr(30)
	cutoff := pgtype.Timestamptz{Time: time.Now().Add(-time.Duration(days) * 24 * time.Hour), Valid: true}
	deleted, err := w.queries.DeleteAuditEventsOlderThan(ctx, cutoff)
	if err != nil {
		log.Printf("audit_cleanup: deleting rows older than %d days: %v", days, err)
		return err
	}
	log.Printf("audit_cleanup: deleted %d rows older than %d days", deleted, days)
	return river.RecordOutput(ctx, &AuditCleanupResult{Deleted: deleted})
}

var _ river.Worker[AuditCleanupArgs] = (*AuditCleanupWorker)(nil)