// Package audit emits structured audit events to the
// okt_system.permission_audit table. The Recorder is the single
// write-side surface; handlers and middlewares call Record (or
// RecordAsync) after a successful mutation, passing the actor
// resolved from the request context and an Event describing the
// mutation.
//
// The package is transport-agnostic: it knows about pgxpool and the
// rbac action constants, nothing about chi, http, or httputil. The
// HTTP layer resolves the actor (httputil.RequestUserID) and the
// repository scope (appmw.RepoIDFromContext) and hands them in.
//
// Writes are best-effort. RecordAsync spawns a goroutine with a
// bounded context and swallows the error (logging it) so an audit
// write can never fail the request that triggered it — the same
// "logged and swallowed" pattern the fetch_attempts JSONB write uses
// (retrieve_source.go:persistFetchAttempts). Synchronous callers
// (tests, the cleanup worker) use Record directly and observe the
// error.
package audit

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// Event describes one audit-worthy mutation. UserID + Username are
// the actor; Username is denormalized so the audit row stays
// readable after the user is deleted (the actor_user_id FK
// ON DELETE SET NULL). RepositoryID is nil/invalid for system
// events and the repo UUID for repo-scoped events. Detail is an
// arbitrary JSON-serializable map (before/after, params, IP, …);
// nil collapses to the table's default '{}'::jsonb. Target is a
// short string identifying the affected entity (id, name, or
// "user_id=…,role=…"); SourceURL is populated only for ingestion
// events.
type Event struct {
	UserID       pgtype.UUID
	Username     string
	Action       string
	Object       string
	RepositoryID pgtype.UUID // zero Valid=false → NULL
	Target       string      // empty → NULL
	Detail       map[string]any
	SourceURL    string // empty → NULL
}

// Recorder is the write-side surface for audit events. The
// interface lets tests substitute a no-op or in-memory recorder
// without touching pgxpool.
type Recorder interface {
	Record(ctx context.Context, e Event) error
	RecordAsync(e Event)
}

// PostgresRecorder writes events to okt_system.permission_audit
// through the sqlc-generated store.Queries. The pool is the system
// pool (the one that backs cfg.System.Database); the audit table is
// always on the system database regardless of which repository the
// event is about, matching the ai_usage precedent.
type PostgresRecorder struct {
	queries *store.Queries
}

// NewPostgresRecorder builds a recorder backed by the given system
// pool. The pool MUST be the system database's pool (the same one
// passed to rbac.SetupRBAC and the NewHandler systemPool argument).
func NewPostgresRecorder(pool *pgxpool.Pool) *PostgresRecorder {
	return &PostgresRecorder{queries: store.New(pool)}
}

// Record writes one event synchronously. Returns the underlying
// pgx error so callers that care (tests, the cleanup worker) can
// observe failures. Detail is marshaled to JSONB; a marshal error
// is reported in place of the pgx error.
func (r *PostgresRecorder) Record(ctx context.Context, e Event) error {
	detail, err := json.Marshal(e.Detail)
	if err != nil {
		return err
	}
	var target *string
	if e.Target != "" {
		t := e.Target
		target = &t
	}
	var sourceURL *string
	if e.SourceURL != "" {
		s := e.SourceURL
		sourceURL = &s
	}
	return r.queries.RecordAuditEvent(ctx, store.RecordAuditEventParams{
		ActorUserID:   e.UserID,
		ActorUsername: e.Username,
		Action:        e.Action,
		Object:        e.Object,
		RepositoryID:  e.RepositoryID,
		Target:        target,
		Detail:        detail,
		SourceUrl:     sourceURL,
	})
}

// RecordAsync writes one event on a background goroutine with a
// bounded context. It never panics and never blocks the caller; the
// write error is logged and swallowed so an audit failure can never
// fail the request that triggered it. Use this from HTTP handlers;
// use Record from tests / workers that want to observe the error.
func (r *PostgresRecorder) RecordAsync(e Event) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := r.Record(ctx, e); err != nil {
			log.Printf("audit: recording %s %s by %q: %v", e.Action, e.Object, e.Username, err)
		}
	}()
}

// NoopRecorder is a Recorder that discards every event. Used in
// tests that don't exercise the audit pipeline (e.g. handler tests
// that only check the HTTP status).
type NoopRecorder struct{}

func (NoopRecorder) Record(context.Context, Event) error  { return nil }
func (NoopRecorder) RecordAsync(Event)                     {}

// Compile-time check that PostgresRecorder and NoopRecorder
// satisfy Recorder.
var (
	_ Recorder = (*PostgresRecorder)(nil)
	_ Recorder = NoopRecorder{}
)