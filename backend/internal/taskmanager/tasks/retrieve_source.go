// Package tasks contains River JobArgs / Worker definitions for
// each background job the application knows how to run. New tasks
// are added here and registered on the River worker bundle in
// taskmanager.New.
package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/content_parsing"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/decomposition"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/fetch"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/registry"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/search"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/storage"
	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/riverqueue/river"
)

// QueueRetrieveSource is the River queue name used for the
// RetrieveSource job. Keep it in sync with `task.queues` in
// configs/config.default.yaml.
const QueueRetrieveSource = "retrieve_source"

// RetrieveSourceArgs is the structured argument passed to the
// RetrieveSource worker. The URL may be either a canonical URL or a
// DOI. Classification happens inside the worker because the caller
// may not yet know the type.
//
// RepositoryID, when set, is the UUID of the repository the
// fetched content should be persisted under. The worker
// resolves the per-repo database pool from the registry and
// writes a row into okt_repository.sources. Leaving it empty
// is supported for jobs that just want to exercise the fetch
// strategy without persisting (e.g. one-off jobs in tests).
//
// DOI is the bare DOI the caller already knows about. It is
// optional; the cheap classifier in fetch.ClassifyURL can
// re-derive it from a doi.org URL, but carrying it explicitly
// lets the search-result click-through path keep the DOI
// even when the URL is a non-DOI form (e.g. an openalex.org
// landing page that the search provider knows resolves to a
// DOI). When set, the worker writes it to the
// okt_repository.sources.doi column on the source row.
//
// PublishedAt is the publication date the caller already
// knows about (e.g. an OpenAlex search result the user
// clicked on). The worker writes it to
// okt_repository.sources.published_at when set; otherwise
// it falls back to parsed.PublishedAt from the
// content_parsing step (trafilatura / htmldate). Day
// precision is what the database column expects, but the
// worker does not enforce it: any time.Time the handler
// forwards is accepted and the time component is dropped
// on persist. Wire shape is an RFC 3339 timestamp; the
// struct mirrors handler.RetrieveSourceArgs one-for-one
// so the taskmanager enqueuer can pass the value through
// unchanged.
//
// Process, when true, asks the worker to enqueue a
// source_decomposition job for the just-fetched row once
// the fetch succeeds and the source has non-empty parsed
// text. This is the "Fetch and Process" path: a single
// user action starts the full retrieve → decompose →
// embed pipeline instead of forcing the user to click
// Process manually after the fetch lands. When the fetch
// fails or the source ends up with no parseable text, the
// worker returns a terminal error so River marks the job
// failed (the user explicitly asked for the full
// pipeline and the first stage did not deliver what was
// needed). A manual Process click on a previously-fetched
// row remains available and is unaffected.
type RetrieveSourceArgs struct {
	URL          string     `json:"url"`
	RepositoryID string     `json:"repository_id,omitempty"`
	DOI          string     `json:"doi,omitempty"`
	PublishedAt  *time.Time `json:"published_at,omitempty"`
	Process      bool       `json:"process,omitempty"`
	// InvestigationID, when set, asks the worker to link the
	// persisted source row into this investigation after
	// persistSource returns. Best-effort and best-effort only:
	// an unknown investigation, a cross-repo investigation, or
	// a junction write error is logged and swallowed so a bad
	// investigation_id never fails the fetch. The MCP
	// fetchAndProcessSource tool uses this to give agents a
	// single-call fetch + organize flow; the REST handler
	// forwards it unchanged when supplied.
	InvestigationID string `json:"investigation_id,omitempty"`
}

// Kind returns the River job kind. Required by River's JobArgs
// interface and is the unique identifier persisted in the database.
func (RetrieveSourceArgs) Kind() string { return "retrieve_source" }

// InsertOpts returns a River retry policy. The default policy is
// exponential backoff, which is what we want for transient network
// failures from the fetch strategy.
func (RetrieveSourceArgs) InsertOpts() river.InsertOpts { return river.InsertOpts{} }

// contentPreviewBytes is the upper bound on the body the worker
// stores on the sources row. Fetch responses for real resources
// can be hundreds of KB to several MB; storing the full payload
// on the per-repo row would balloon the table and slow down
// list queries. We keep a head-prefix instead, which is enough
// for a future "preview" UI to render and for downstream
// classification features to act on without round-tripping
// the original URL. A future migration can move the full body
// to object storage and reduce this column to a pointer.
const contentPreviewBytes = 32 * 1024

// RetrieveSourceResult is the result blob River stores on the job
// row. Right now it just records what we ended up doing so an
// operator can look at the row and understand what happened.
type RetrieveSourceResult struct {
	ClassifiedAs fetch.SourceType `json:"classified_as"`
	Value        string           `json:"value"`
	Fetched      bool             `json:"fetched"`
	StatusCode   int              `json:"status_code,omitempty"`
	Bytes        int              `json:"bytes,omitempty"`
	FinalURL     string           `json:"final_url,omitempty"`
	Searched     bool             `json:"searched"`
	SearchHits   int              `json:"search_hits,omitempty"`
	// SourceID is the UUID of the row the worker created in
	// okt_repository.sources. Empty when the job ran without a
	// repository (RepositoryID was empty or the lookup failed).
	// Operators looking at a completed job can use it to find
	// the matching row in the per-repo table.
	SourceID string `json:"source_id,omitempty"`
}

// RetrieveSourceWorker is the River worker that classifies a URL,
// tries a search provider to enrich classification if necessary, and
// then uses the fetch strategy to pull the resource. The fetch and
// search providers are injected at construction time so the worker
// reuses the same instances the rest of the application does.
//
// The worker also persists a row in the active repository's
// `sources` table so the UI can list what has been fetched.
// Pool resolution mirrors the per-repo HTTP middleware: the
// system pool resolves the repository's `database_name` and
// the registry returns the matching per-repo pool.
type RetrieveSourceWorker struct {
	river.WorkerDefaults[RetrieveSourceArgs]

	searchProviders map[string]search.SearchProvider
	fetchStrategy   *fetch.FetchStrategy
	registry        *dbpool.Registry
	systemQueries   *store.Queries
	storage         storage.FileStorage
	registryClients *registry.ClientMap
	qdrant          *qdrantstore.Store
	reconciler      *CacheReconciler
}

// NewRetrieveSourceWorker builds a worker. The search provider map
// is the same one the API serves via /sources/providers; in local
// dev it's a single Serper or OpenAlex instance keyed by id.
//
// `registry` and `systemQueries` are used to resolve the
// per-repo pool the worker writes into. The worker only
// needs them when the job carries a RepositoryID; when the
// field is empty the worker skips persistence entirely.
//
// `storage` is the file-storage backend the worker uses to
// persist inline images, PDF page renders, and (for PDF sources)
// the full source body. It may be nil — the worker skips the
// storage step in that case and the rows stay with NULL
// storage_key, which is the v1 behavior. The serving endpoints
// return 404 for un-stored assets.
//
// `registryClients` is the per-registry client map built from
// cfg.Providers.ResolveRegistries. The worker resolves the repo's
// client from its registry_id column and skips the cache lookup
// when the per-repo registry_enabled flag is false. Nil is safe —
// every registry path is a no-op.
func NewRetrieveSourceWorker(
	searchProviders map[string]search.SearchProvider,
	fetchStrategy *fetch.FetchStrategy,
	poolRegistry *dbpool.Registry,
	systemQueries *store.Queries,
	stor storage.FileStorage,
	registryClients *registry.ClientMap,
	qdrant *qdrantstore.Store,
	reconciler *CacheReconciler,
) *RetrieveSourceWorker {
	return &RetrieveSourceWorker{
		searchProviders: searchProviders,
		fetchStrategy:   fetchStrategy,
		registry:        poolRegistry,
		systemQueries:   systemQueries,
		storage:         stor,
		registryClients: registryClients,
		qdrant:          qdrant,
		reconciler:      reconciler,
	}
}

// Work classifies the input URL and, when supported, runs the fetch
// strategy to pull the content. Failures bubble up so River can
// apply its retry policy; the job is considered failed after
// MaxAttempts.
func (w *RetrieveSourceWorker) Work(ctx context.Context, job *river.Job[RetrieveSourceArgs]) error {
	args := job.Args

	// The job accepts either a URL or a bare DOI. The REST
	// EnqueueRetrieveSource handler always sends a URL (it 400s on
	// an empty one), but the MCP fetchAndProcessSource tool allows a
	// DOI-only call — an agent that ran searchSources and got back
	// hits with `doi` but no canonical `url` enqueues the DOI
	// directly. Without accepting the DOI here those jobs fail
	// instantly with "url is required" and River retries them up
	// to MaxAttempts, producing the stuck-task storm the operator
	// sees in the logs.
	if args.URL == "" && args.DOI == "" {
		return errors.New("retrieve_source: url or doi is required")
	}

	var resource fetch.Resource
	if args.URL != "" {
		resource = fetch.ClassifyURL(args.URL)
	} else {
		// DOI-only job. Synthesize the SourceDOI resource the
		// fetch strategy expects so the Unpaywall / doi.org
		// tiers handle it the same way a doi.org URL would.
		resource = fetch.Resource{Value: args.DOI, Type: fetch.SourceDOI, DOI: args.DOI}
	}
	// Carry a caller-supplied DOI through to the persistence
	// step even when the classifier couldn't extract one from
	// the URL. This is the search-result click-through path:
	// the user picked a hit whose `doi` field is known, but
	// the URL is e.g. an openalex.org/W… landing page that
	// the cheap classifier treats as a plain URL.
	//
	// When the caller knows the DOI we additionally force the
	// resource Type to SourceDOI so the fetch strategy tries
	// the DOI path (Unpaywall OA lookup) first, instead of
	// fetching the OpenAlex landing page as a plain URL and
	// never reaching Unpaywall. The URL is still recorded on
	// the source row for display; the DOI is what drives the
	// fetch. This matches the reference Python implementation,
	// which routes doi.org URLs straight to the DOI provider.
	if resource.DOI == "" && args.DOI != "" {
		resource.DOI = args.DOI
	}
	if args.DOI != "" && resource.Type != fetch.SourceDOI {
		resource.Type = fetch.SourceDOI
		resource.Value = args.DOI
	}
	result := RetrieveSourceResult{
		ClassifiedAs: resource.Type,
		Value:        resource.Value,
	}

	log.Printf("retrieve_source: classified %q as %s=%q", args.URL, resource.Type, resource.Value)

	// If the search provider could enrich the classification (e.g.
	// resolve a bare query to a canonical URL/DOI), do that first.
	// The current implementation defers to the fetch strategy; the
	// extension point lives here for when we add a real
	// search-driven resolution path. We reference the field so the
	// linter / future readers see that it's intentionally
	// available on the worker.
	_ = w.searchProviders

	// Registry pre-check. When the registry is configured and
	// has a source matching this URL/DOI, the worker imports
	// the pre-computed artifacts (facts, concepts, summaries)
	// and skips the fetch + decomposition pipeline entirely.
	// This is the "cache hit" path: the same source was already
	// decomposed by another OKT instance and its artifacts are
	// stored in the registry's S3/R2.
	imported, importStats, importErr := w.tryRegistryImport(ctx, args, resource, &result)
	if importErr != nil {
		log.Printf("retrieve_source: registry import attempted but failed (falling through to fetch): %v", importErr)
	}
	if imported {
		log.Printf("retrieve_source: source imported from registry (created=%d skipped=%d, skipping fetch)",
			importStats.Created, importStats.Skipped)
		// Delta-aware reconciliation: the CacheReconciler decides
		// whether to enqueue downstream jobs based on whether the
		// import produced any new facts (created > 0) and whether
		// the imported embedding model matches the local config.
		// An already-synced source (all facts skipped) produces an
		// empty plan and triggers zero jobs. Summarize is NOT
		// enqueued directly — it's reached transitively via
		// dedup → extract_concepts → summarize, which only fires
		// when dedup promotes new stable facts.
		plan := w.reconciler.Plan(importStats)
		if plan.ReembedFacts {
			// Reset embeddings on the imported facts so the
			// embed_facts worker re-embeds them with the local
			// model (the imported vectors are in a foreign space).
			repoID := pgtype.UUID{}
			if err := repoID.Scan(args.RepositoryID); err == nil {
				dbName, dbErr := w.systemQueries.GetRepositoryDatabaseName(ctx, repoID)
				if dbErr == nil {
					pool := w.registry.Get(dbName)
					w.reconciler.ResetForReembed(ctx, store.New(pool.Pool), repoID)
				}
			}
		}
		EnqueuePlan(ctx, plan, args.RepositoryID, []string{result.SourceID})
		return river.RecordOutput(ctx, &result)
	}

	// Resolve the resource. The fetch strategy returns a
	// ResolvedContent for any successful fetch (2xx with
	// sufficient extractable content) and an error
	// otherwise — including non-2xx HTTP statuses (which
	// the providers now surface as ErrNon2xxStatus so the
	// chain can fall through) and the
	// ErrInsufficientContent sentinel (which the strategy
	// collapses back to a successful result when no
	// heavier tier upgraded the content). A non-nil err
	// means the row should be marked failed; a nil err
	// means the content is ready to persist.
	// The budget must accommodate the full provider chain
	// (Unpaywall OA fetch up to 60s + TLS 60s + fetch 60s +
	// FlareSolverr 60s + OA URL second pass up to 180s),
	// so 300s ensures every tier including FlareSolverr's
	// headless browser (60s internal timeout) gets a real
	// chance without starving the fallbacks.
	fetchCtx, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()

	content, err := w.fetchStrategy.Resolve(fetchCtx, resource)
	if err != nil {
		log.Printf("retrieve_source: fetch failed for %s=%q: %v", resource.Type, resource.Value, err)
	} else {
		result.StatusCode = content.StatusCode
		result.FinalURL = content.FinalURL
		result.Fetched = true
		result.Bytes = len(content.Body)
		log.Printf("retrieve_source: fetched %d bytes from %s (status %d)",
			result.Bytes, result.FinalURL, result.StatusCode)
	}

	// Persist the row in the per-repo sources table. We do this
	// regardless of fetch success: a 'failed' row is still useful
	// for the UI to show "we tried, here's why it broke". The
	// work is best-effort: if the pool lookup fails (unknown
	// repository, missing wiring) we log and continue rather than
	// failing the job, so a misconfigured job doesn't tank the
	// queue.
	//
	// displayURL is the value written to the sources.url column
	// (and the key for the (repository_id, url) UNIQUE
	// constraint). For URL-bearing jobs it's the caller's URL
	// unchanged. For DOI-only MCP jobs args.URL is empty; using
	// "" would collapse every DOI-only job for a repo onto one
	// row (the unique constraint would turn the second enqueue
	// into an overwrite) and leave the UI with no clickable
	// link. The doi.org/<doi> form is what ClassifyURL would
	// produce for the equivalent doi.org URL, so it keeps the
	// row consistent with the URL-bearing path and gives the
	// frontend a real link.
	displayURL := args.URL
	if displayURL == "" && args.DOI != "" {
		displayURL = "https://doi.org/" + args.DOI
	}
	var parsedHasText bool
	if w.registry != nil && w.systemQueries != nil && args.RepositoryID != "" {
		sourceID, persistErr := w.persistSource(ctx, args.RepositoryID, displayURL, resource, content, err, args.PublishedAt)
		if persistErr != nil {
			log.Printf("retrieve_source: persisting source row failed: %v", persistErr)
		} else {
			result.SourceID = sourceID
			// When the caller asked us to chain into
			// decomposition, we need to know whether the row
			// ended up 'fetched' with non-empty parsed text —
			// the same precondition the manual Process handler
			// enforces. Re-reading the row we just wrote is the
			// simplest correct check; persistSource already
			// resolved the per-repo pool, so we resolve it
			// again here (cheap, registry-cached) and run a
			// single GetSourceByID.
			if args.Process && result.Fetched {
				parsedHasText = w.sourceHasParsedText(ctx, args.RepositoryID, sourceID)
			}
			// Best-effort investigation link. When the caller
			// (today: the MCP fetchAndProcessSource tool with
			// its optional investigationId) asks the worker to
			// organize the just-persisted source into an
			// investigation, insert the junction row here —
			// the source_id only exists once persistSource
			// returns, so this is the earliest point the link
			// can be recorded. A failed fetch still creates a
			// row (status='failed'), and we link it so the
			// agent/user can see the attempted source in the
			// investigation's list. Ownership is re-verified
			// (the investigation must belong to the same
			// repository) to defend against a stale id; the
			// MCP handler validates up front but a direct
			// River.Insert caller could bypass it. Errors are
			// logged and swallowed — a bad investigation_id
			// must not fail the fetch.
			if args.InvestigationID != "" {
				w.linkSourceToInvestigation(ctx, args.RepositoryID, sourceID, args.InvestigationID)
			}
		}
	}

	// "Fetch and Process" chain. When the caller set Process we
	// enqueue a source_decomposition job for the row we just
	// persisted, so the user gets the full retrieve → decompose
	// → embed pipeline from a single action. The chain mirrors
	// the source_decomposition → embed_facts chain: it uses
	// river.ClientFromContext so the worker stays decoupled from
	// the Manager, and a fresh context.Background() with a short
	// timeout because River cancels the worker ctx on completion
	// (reusing it races the cancellation on the chained BEGIN).
	//
	// Precondition: the fetch succeeded and the row has
	// non-empty parsed text. When the fetch failed the row is
	// already stamped 'failed' and chaining would just produce a
	// no-op decompose job; more importantly, the user explicitly
	// asked for the full pipeline, so a failed fetch is a failed
	// pipeline — we return a terminal error and let River mark
	// the job failed. When the fetch succeeded but parsing
	// produced no text (e.g. a 2xx response the content parser
	// couldn't claim), the row is 'fetched' with NULL parsed_text
	// and there is nothing to decompose; the same terminal-error
	// rule applies so the user is not silently left with a
	// half-done pipeline.
	if args.Process {
		if !result.Fetched {
			return river.RecordOutput(ctx, &result)
		}
		if !parsedHasText {
			if result.SourceID == "" {
				// Persistence was best-effort and skipped
				// (no registry / no RepositoryID). Without
				// a row there's nothing to chain and nothing
				// to mark failed in the per-repo table; just
				// surface the terminal error so River records
				// the job as failed.
				return fmt.Errorf("retrieve_source: process requested but source row was not persisted (no repository wiring)")
			}
			return fmt.Errorf("retrieve_source: process requested but source %s has no parseable text", result.SourceID)
		}
		if result.SourceID == "" || args.RepositoryID == "" {
			return fmt.Errorf("retrieve_source: process requested but source_id/repository_id missing")
		}
		client := river.ClientFromContext[pgx.Tx](ctx)
		if client == nil {
			log.Printf("retrieve_source: no river client on context; source_decomposition not enqueued for source %s (row is fetched)", result.SourceID)
		} else {
		chainCtx, chainCancel := context.WithTimeout(context.Background(), 15*time.Second)
		if _, err := client.Insert(chainCtx, SourceDecompositionArgs{
			SourceID:     result.SourceID,
			RepositoryID: args.RepositoryID,
		}, &river.InsertOpts{
			Queue: QueueSourceDecomposition,
			Metadata: MarshalMetadata(JobMetadata{
				RepositoryID: args.RepositoryID,
				SourceID:     result.SourceID,
			}),
		}); err != nil {
				log.Printf("retrieve_source: enqueueing source_decomposition for source %s failed: %v", result.SourceID, err)
			} else {
				log.Printf("retrieve_source: enqueued source_decomposition for source %s", result.SourceID)
			}
			chainCancel()
		}
	}

	// Persist a short summary back to the job row so operators can
	// inspect it without digging through logs.
	return river.RecordOutput(ctx, &result)
}

// sourceHasParsedText resolves the per-repo pool for the given
// repository and reports whether the source row with the given ID
// has any non-empty parsed content column (parsed_markdown first,
// falling back to parsed_text). It is used by the "Fetch and
// Process" chain to decide whether the just-fetched row is
// eligible for decomposition (mirroring the precondition the
// manual ProcessSource HTTP handler enforces). Any error is
// treated as "no text" so the chain fails loudly rather than
// enqueuing a no-op decompose job. The Markdown-first check keeps
// the chain firing for newly-fetched rows while still accepting
// legacy rows populated with parsed_text only.
func (w *RetrieveSourceWorker) sourceHasParsedText(ctx context.Context, repoIDStr, sourceIDStr string) bool {
	repoID := pgtype.UUID{}
	if err := repoID.Scan(repoIDStr); err != nil {
		log.Printf("retrieve_source: parsing repository id for process check: %v", err)
		return false
	}
	dbName, err := w.systemQueries.GetRepositoryDatabaseName(ctx, repoID)
	if err != nil {
		log.Printf("retrieve_source: resolving repository database for process check: %v", err)
		return false
	}
	pool := w.registry.Get(dbName)
	queries := store.New(pool.Pool)
	sourceID := pgtype.UUID{}
	if err := sourceID.Scan(sourceIDStr); err != nil {
		log.Printf("retrieve_source: parsing source id for process check: %v", err)
		return false
	}
	src, err := queries.GetSourceByID(ctx, sourceID)
	if err != nil {
		log.Printf("retrieve_source: loading source for process check: %v", err)
		return false
	}
	return (src.ParsedMarkdown != nil && *src.ParsedMarkdown != "") ||
		(src.ParsedText != nil && *src.ParsedText != "")
}

// linkSourceToInvestigation inserts the investigation_sources
// junction row linking the just-persisted source into the
// investigation the caller named via RetrieveSourceArgs.
// InvestigationID. Best-effort: any error (unknown
// investigation, cross-repo investigation, junction write
// failure) is logged and swallowed so a bad investigation_id
// never fails the fetch — the row is still useful on its own.
//
// The ownership guard mirrors the REST Investigations.AddSource
// endpoint: the investigation must belong to the same repository
// as the source. The MCP handler validates up front, but a
// direct River.Insert caller could bypass it, so the worker
// re-checks here. Idempotent via the query's ON CONFLICT DO
// NOTHING, so a re-fetch of the same URL (which reuses the
// source row via the (repository_id, url) UNIQUE constraint)
// does not fail on a duplicate junction insert.
func (w *RetrieveSourceWorker) linkSourceToInvestigation(ctx context.Context, repoIDStr, sourceIDStr, investigationIDStr string) {
	repoID := pgtype.UUID{}
	if err := repoID.Scan(repoIDStr); err != nil {
		log.Printf("retrieve_source: parsing repository id for investigation link: %v", err)
		return
	}
	dbName, err := w.systemQueries.GetRepositoryDatabaseName(ctx, repoID)
	if err != nil {
		log.Printf("retrieve_source: resolving repository database for investigation link: %v", err)
		return
	}
	pool := w.registry.Get(dbName)
	queries := store.New(pool.Pool)

	invID := pgtype.UUID{}
	if err := invID.Scan(investigationIDStr); err != nil {
		log.Printf("retrieve_source: parsing investigation id for link: %v", err)
		return
	}
	// Verify the investigation belongs to the same repo. A
	// cross-repo or unknown investigation_id is logged and
	// skipped — the fetch itself is unaffected.
	inv, err := queries.GetInvestigationByID(ctx, invID)
	if err != nil {
		log.Printf("retrieve_source: loading investigation %s for link: %v", investigationIDStr, err)
		return
	}
	if inv.RepositoryID != repoID {
		log.Printf("retrieve_source: investigation %s does not belong to repo %s; skipping link", investigationIDStr, repoIDStr)
		return
	}

	sourceID := pgtype.UUID{}
	if err := sourceID.Scan(sourceIDStr); err != nil {
		log.Printf("retrieve_source: parsing source id for investigation link: %v", err)
		return
	}
	if err := queries.AddInvestigationSource(ctx, store.AddInvestigationSourceParams{
		InvestigationID: invID,
		SourceID:         sourceID,
	}); err != nil {
		log.Printf("retrieve_source: linking source %s to investigation %s failed: %v", sourceIDStr, investigationIDStr, err)
		return
	}
	log.Printf("retrieve_source: linked source %s to investigation %s", sourceIDStr, investigationIDStr)
}

// tryRegistryImport checks the registry for a source matching the
// given resource and, when found, imports the pre-computed artifacts
// (source row, facts, concepts, summaries, embeddings) into the
// local database and returns true. When the registry is not configured,
// returns false without error. Any failure is logged and returns false
// so the caller falls through to the normal fetch path. The returned
// ImportStats tells the CacheReconciler whether the import produced
// any delta and which embedding models were used.
func (w *RetrieveSourceWorker) tryRegistryImport(
	ctx context.Context,
	args RetrieveSourceArgs,
	resource fetch.Resource,
	result *RetrieveSourceResult,
) (bool, ImportStats, error) {
	var stats ImportStats
	if w.registryClients == nil || !w.registryClients.IsConfigured() {
		return false, stats, nil
	}

	// Registry needs a repo ID to search in. If the job doesn't
	// have one, there's no point looking — we'd have nowhere to
	// import artifacts.
	if args.RepositoryID == "" {
		return false, stats, nil
	}

	repoID := pgtype.UUID{}
	if err := repoID.Scan(args.RepositoryID); err != nil {
		return false, stats, nil
	}

	// Per-repo gate: the repo may have turned the registry
	// integration off (registry_enabled=false). Skip the cache
	// lookup in that case and fall through to the fetch path.
	regCfg, err := w.systemQueries.GetRepositoryRegistryConfig(ctx, repoID)
	if err != nil {
		return false, stats, nil // transient DB issue — don't block the fetch
	}
	if !regCfg.RegistryEnabled {
		return false, stats, nil
	}

	regID := "default"
	if regCfg.RegistryID != nil && *regCfg.RegistryID != "" {
		regID = *regCfg.RegistryID
	}
	rc, _, ok := w.registryClients.Client(regID)
	if !ok || !rc.IsConfigured() {
		return false, stats, nil
	}

	searchURL := args.URL
	searchDOI := args.DOI
	if searchDOI == "" {
		searchDOI = resource.DOI
	}

	sr, err := rc.SearchSource(ctx, searchURL, searchDOI)
	if err != nil {
		return false, stats, fmt.Errorf("registry search: %w", err)
	}
	if sr == nil || !sr.Found {
		return false, stats, nil
	}

	log.Printf("retrieve_source: registry hit for source %q (id=%s)", sr.Title, sr.SourceID)

	pkg, err := rc.PullSource(ctx, sr.SourceID)
	if err != nil {
		return false, stats, fmt.Errorf("registry pull source: %w", err)
	}

	// Per-repo pull level (migration 0044). Controls whether the
	// import includes concepts/links/concept-embeddings or only
	// sources + facts + fact embeddings. Defaults to "concepts".
	syncLevels, err := w.systemQueries.GetRepositorySyncLevels(ctx, repoID)
	if err != nil {
		return false, stats, fmt.Errorf("registry sync levels read: %w", err)
	}
	pullFilter := registry.NewSyncLevelFilter(registry.ParseSyncLevel(syncLevels.RegistryPullLevel))

	// Import the source row and decomposition artifacts.
	sourceID, importStats, err := w.importFromRegistry(ctx, args, resource, pkg, result, rc, pullFilter)
	if err != nil {
		return false, importStats, fmt.Errorf("registry import: %w", err)
	}
	result.SourceID = sourceID
	result.Fetched = true
	result.Searched = true

	// Best-effort investigation link, same as the fetch path.
	if args.InvestigationID != "" {
		w.linkSourceToInvestigation(ctx, args.RepositoryID, sourceID, args.InvestigationID)
	}

	return true, importStats, nil
}

// importFromRegistry pulls the source package + decomposition from
// the registry, creates/updates the source row, and persists the
// pre-computed facts, concepts, links, aliases, and embeddings
// into the local database and Qdrant. It is delta-aware: for each
// fact it checks whether an identical fact (same text) is already
// linked to this source; if so it skips CreateFact entirely (no new
// row, no Qdrant point, no downstream job). The returned ImportStats
// tells the CacheReconciler whether to enqueue downstream jobs and
// whether the imported embedding model differs from the local config.
//
// Qdrant points are keyed by the LOCAL fact/concept UUID (not the
// registry's embedding UUID) so Postgres and Qdrant stay consistent
// and a fact's vector can be deleted by its Postgres id. The
// embedded_model column is stamped with emb.Model (the actual
// embedding model that produced the vector), not decomp.ModelID
// (the generation model), so the reconciler can detect a mismatch.
func (w *RetrieveSourceWorker) importFromRegistry(
	ctx context.Context,
	args RetrieveSourceArgs,
	resource fetch.Resource,
	pkg *registry.SourcePackage,
	result *RetrieveSourceResult,
	rc *registry.Client,
	pullFilter *registry.SyncLevelFilter,
) (string, ImportStats, error) {
	stats := ImportStats{}
	repoID := pgtype.UUID{}
	if err := repoID.Scan(args.RepositoryID); err != nil {
		return "", stats, fmt.Errorf("invalid repository id %q: %w", args.RepositoryID, err)
	}

	dbName, err := w.systemQueries.GetRepositoryDatabaseName(ctx, repoID)
	if err != nil {
		return "", stats, fmt.Errorf("resolving repository database: %w", err)
	}

	pool := w.registry.Get(dbName)
	queries := store.New(pool.Pool)

	// Create or reuse the source row, same as persistSource.
	displayURL := args.URL
	if displayURL == "" && args.DOI != "" {
		displayURL = "https://doi.org/" + args.DOI
	}

	id := pgtype.UUID{}
	if err := id.Scan(uuid.New().String()); err != nil {
		return "", stats, fmt.Errorf("generating source id: %w", err)
	}
	kind := string(resource.Type)
	if kind == "" {
		kind = "url"
	}
	var doi *string
	if pkg.Source.DOI != "" {
		d := pkg.Source.DOI
		doi = &d
	} else if resource.DOI != "" {
		d := resource.DOI
		doi = &d
	}

	title := pkg.Source.Title
	urlVal := displayURL
	if urlVal == "" {
		urlVal = pkg.Source.URL
	}

	_, insertErr := queries.CreateSource(ctx, store.CreateSourceParams{
		ID:           id,
		RepositoryID: repoID,
		Url:          urlVal,
		Kind:         kind,
		Status:       "fetching",
		Doi:          doi,
	})
	if insertErr != nil {
		existing, listErr := queries.GetSourceByRepoAndURL(ctx, store.GetSourceByRepoAndURLParams{
			RepositoryID: repoID,
			Url:          urlVal,
		})
		if listErr != nil {
			return "", stats, fmt.Errorf("create or lookup source: create=%v lookup=%w", insertErr, listErr)
		}
		id = existing.ID
	}

	_, err = queries.MarkSourceFetched(ctx, store.MarkSourceFetchedParams{
		ID:      id,
		Content: nil,
	})
	if err != nil {
		return "", stats, fmt.Errorf("marking source fetched: %w", err)
	}
	_, parseErr := queries.MarkSourceParsed(ctx, store.MarkSourceParsedParams{
		ID:          id,
		ParsedTitle: &title,
		ParseStatus: strPtr("ok"),
	})
	if parseErr != nil {
		log.Printf("retrieve_source: marking registry source parsed: %v", parseErr)
	}

	// Resolve the per-repo model whitelist. When the repo has set
	// allowed_models (non-NULL), it replaces the global config; when
	// NULL, the global registry config is the fallback. This is the
	// "per-repo replaces global" semantics.
	allowedModels := resolveAllowedModels(ctx, w.systemQueries, repoID, rc.AllowedModels())

	// Import each allowed decomposition model.
	for _, dr := range pkg.Decompositions {
		if !registry.IsAllowed(allowedModels, dr.ModelID) {
			continue
		}
		decomp, err := rc.PullDecomposition(ctx, pkg.Source.ID, dr.ModelID)
		if err != nil {
			log.Printf("retrieve_source: pulling decomposition %s from registry: %v", dr.ModelID, err)
			continue
		}
		// Strip concept-level fields when the repo's pull level is
		// "facts". The concept/link/concept-embedding import loops
		// below then iterate zero items, leaving fact_concepts empty
		// so extract_concepts regenerates concepts from the stable
		// facts. One line per pull path — the filter is the single
		// source of truth for what each level includes.
		decomp = pullFilter.FilterForPull(decomp)

		// Track fact content_hash → local fact_id for link resolution
		// and Qdrant point mapping. The registry's embedding key
		// carries a remote UUID; we map it to the local fact UUID so
		// Qdrant points are keyed by the same id as the Postgres row.
		factIDByHash := make(map[string]pgtype.UUID, len(decomp.Facts))
		// conceptKey → local concept UUID, built during the concept
		// import loop so the embedding loop can resolve concept
		// points to local UUIDs.
		conceptIDByKey := make(map[string]pgtype.UUID, len(decomp.Concepts))
		// embKey → local UUID (fact or concept), built during the
		// fact/concept import loops so the embedding loop can upsert
		// Qdrant points with local UUIDs.
		localUUIDByEmbKey := make(map[string]pgtype.UUID)
		// The actual embedding model used by this decomposition's
		// vectors (read from EmbeddingData.Model, not decomp.ModelID
		// which is the extraction model). All embeddings in one
		// decomposition share the same model.
		var decompEmbModel string
		var decompEmbDims int
		if decomp.Embeddings != nil {
			decompEmbModel = decomp.Embeddings.Model
			decompEmbDims = decomp.Embeddings.Dimensions
		}

		// Import facts + link to source. Delta-aware: skip facts
		// whose exact text is already linked to this source (a
		// re-import of an already-synced source is a no-op).
		for _, f := range decomp.Facts {
			// Exact-text no-op check: if this fact already exists
			// linked to this source, skip entirely. After a prior
			// import + dedup, the survivor is re-linked to the
			// source via mergeSources, so this finds it.
			existing, err := queries.GetFactByTextAndSource(ctx, store.GetFactByTextAndSourceParams{
				Text:     f.Content,
				SourceID: id,
			})
			if err == nil {
				// Already exists — record for link resolution but
				// don't create a new row or Qdrant point.
				if f.ContentHash != "" {
					factIDByHash[f.ContentHash] = existing.ID
				}
				stats.Skipped++
				continue
			}

			factID := pgtype.UUID{}
			if err := factID.Scan(uuid.New().String()); err != nil {
				log.Printf("retrieve_source: generating fact id: %v", err)
				continue
			}
			factKind := "text"
			if f.ImageURL != "" {
				factKind = "image"
			}
			if _, err := queries.CreateFact(ctx, store.CreateFactParams{
				ID:       factID,
				Text:     f.Content,
				FactKind: factKind,
				ImageUrl: strPtrOrNil(f.ImageURL),
			}); err != nil {
				log.Printf("retrieve_source: creating fact from registry: %v", err)
				continue
			}
			if f.ContentHash != "" {
				factIDByHash[f.ContentHash] = factID
			}
			if err := queries.AddFactSource(ctx, store.AddFactSourceParams{
				FactID:     factID,
				SourceID:   id,
				ChunkIndex: int32(f.SentenceIdx),
			}); err != nil {
				log.Printf("retrieve_source: linking fact to source: %v", err)
			}
			stats.Created++
		}

		// Import concepts + aliases.
		for _, c := range decomp.Concepts {
			if c.CanonicalName == "" {
				continue
			}
			desc := strPtrOrNil(c.Context)
			if _, err := queries.CreateConcept(ctx, store.CreateConceptParams{
				RepositoryID:  repoID,
				CanonicalName: c.CanonicalName,
				Context:       c.Context,
				Description:   desc,
			}); err != nil {
				log.Printf("retrieve_source: creating concept from registry: %v", err)
				continue
			}
			// Resolve the concept we just created (or that already
			// existed — CreateConcept is ON CONFLICT DO NOTHING)
			// for alias + link import + Qdrant point mapping.
			concept, err := queries.GetConceptByNameContext(ctx, store.GetConceptByNameContextParams{
				RepositoryID:  repoID,
				CanonicalName: c.CanonicalName,
				Context:       c.Context,
			})
			if err != nil {
				log.Printf("retrieve_source: resolving concept %q/%q: %v", c.CanonicalName, c.Context, err)
				continue
			}
			// Registry-imported concepts come pre-refined with
			// canonical names + aliases. Mark them so refine_concepts
			// skips them.
			if err := queries.SetConceptRefinedAt(ctx, concept.ID); err != nil {
				log.Printf("retrieve_source: setting refined_at for concept %s: %v", pgUUIDToString(concept.ID), err)
			}
			// Map the concept's embedding key (from decomp.Embeddings,
			// which the registry keys by the concept's remote UUID)
			// to the local concept UUID. The embedding ref carries the
			// content_hash that links it to this concept when the
			// concept has an embedding in the registry payload.
			conceptKey := c.CanonicalName + "\x00" + c.Context
			conceptIDByKey[conceptKey] = concept.ID
			for _, alias := range c.Aliases {
				if alias == "" {
					continue
				}
				if _, err := queries.AddConceptAlias(ctx, store.AddConceptAliasParams{
					ConceptID: concept.ID,
					AliasText: alias,
				}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
					log.Printf("retrieve_source: adding alias %q for concept %s: %v", alias, pgUUIDToString(concept.ID), err)
				}
			}
		}

		// Import fact_concept links.
		for _, link := range decomp.Links {
			factID, ok := factIDByHash[link.FactContentHash]
			if !ok {
				continue
			}
			concept, err := queries.GetConceptByNameContext(ctx, store.GetConceptByNameContextParams{
				RepositoryID:  repoID,
				CanonicalName: link.ConceptName,
				Context:       link.ConceptContext,
			})
			if err != nil {
				log.Printf("retrieve_source: resolving concept for link %q/%q: %v", link.ConceptName, link.ConceptContext, err)
				continue
			}
			if _, err := queries.AddFactConcept(ctx, store.AddFactConceptParams{
				FactID:    factID,
				ConceptID: concept.ID,
			}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				log.Printf("retrieve_source: adding fact_concept link: %v", err)
			}
		}

		// Resolve embedding keys to local UUIDs. The registry's
		// EmbeddingData.Vectors is keyed by "fact:<remote_uuid>" or
		// "concept:<remote_uuid>"; we need the LOCAL fact/concept
		// UUID so Qdrant points are keyed by the same id as the
		// Postgres row. Since the registry's EmbeddingData doesn't
		// carry per-vector content_hash (it's a map of key→vector),
		// we resolve by matching the key's UUID portion against the
		// fact's content_hash → local ID map, or by iterating
		// concepts for concept keys.
		if decomp.Embeddings != nil {
			for embKey := range decomp.Embeddings.Vectors {
				parts := strings.SplitN(embKey, ":", 2)
				if len(parts) != 2 {
					continue
				}
				// The push path keys fact embeddings by
				// "fact:<content_hash>" so we can resolve them
				// via factIDByHash. Concept embeddings are keyed
				// by "concept:<uuid>" and matched best-effort.
				var localID pgtype.UUID
				var found bool
				if parts[0] == "fact" {
					if fID, ok := factIDByHash[parts[1]]; ok {
						localID = fID
						found = true
					}
				}
				if found {
					localUUIDByEmbKey[embKey] = localID
				}
			}
		}

		// Import embeddings into Qdrant using LOCAL UUIDs.
		if w.qdrant != nil && decomp.Embeddings != nil {
			var factPoints []qdrantstore.FactPoint
			var conceptPoints []qdrantstore.ConceptPoint
			for embKey, values := range decomp.Embeddings.Vectors {
				localID, ok := localUUIDByEmbKey[embKey]
				if !ok {
					continue
				}
				localUUID, err := uuid.Parse(pgUUIDToString(localID))
				if err != nil {
					continue
				}
				vec := make([]float32, len(values))
				for i, v := range values {
					vec[i] = float32(v)
				}
				parts := strings.SplitN(embKey, ":", 2)
				switch parts[0] {
				case "fact":
					factPoints = append(factPoints, qdrantstore.FactPoint{
						ID:           localUUID,
						Vector:       vec,
						RepositoryID: pgtypeToUUID(repoID),
						Status:       "new",
					})
				case "concept":
					conceptPoints = append(conceptPoints, qdrantstore.ConceptPoint{
						ID:           localUUID,
						Vector:       vec,
						RepositoryID: pgtypeToUUID(repoID),
					})
				}
			}
			if len(factPoints) > 0 {
				if err := w.qdrant.UpsertFactVectors(ctx, factPoints); err != nil {
					log.Printf("retrieve_source: upserting fact vectors: %v", err)
				}
			}
			if len(conceptPoints) > 0 {
				if err := w.qdrant.UpsertConceptVectors(ctx, conceptPoints); err != nil {
					log.Printf("retrieve_source: upserting concept vectors: %v", err)
				}
			}
			// Mark facts and concepts as embedded in Postgres. Use
			// the actual embedding model (emb.Model), not the
			// generation model (decomp.ModelID), so the reconciler
			// can detect a mismatch with the local embedding config.
			embModelPtr := strPtrOrNil(decompEmbModel)
			for _, f := range decomp.Facts {
				if fID, ok := factIDByHash[f.ContentHash]; ok {
					if _, err := queries.MarkFactEmbedded(ctx, store.MarkFactEmbeddedParams{
						ID:            fID,
						EmbeddedModel: embModelPtr,
					}); err != nil {
						log.Printf("retrieve_source: marking fact embedded: %v", err)
					}
				}
			}
			for _, c := range decomp.Concepts {
				if c.CanonicalName == "" {
					continue
				}
				conceptKey := c.CanonicalName + "\x00" + c.Context
				conceptID, ok := conceptIDByKey[conceptKey]
				if !ok {
					continue
				}
				if _, err := queries.MarkConceptEmbedded(ctx, store.MarkConceptEmbeddedParams{
					ID:            conceptID,
					EmbeddedModel: embModelPtr,
				}); err != nil {
					log.Printf("retrieve_source: marking concept embedded: %v", err)
				}
			}
		}

		// Track the embedding model for the reconciler's mismatch
		// detection.
		if decompEmbModel != "" {
			stats.ImportedEmbModels = append(stats.ImportedEmbModels, decompEmbModel)
			stats.ImportedEmbDims = append(stats.ImportedEmbDims, decompEmbDims)
		}
	}

	return uuidFromPgtype(id), stats, nil
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// persistSource creates (or reuses) a row in okt_repository.sources
// for the given repository and URL, then updates it with the fetch
// outcome. The flow is:
//
//  1. Resolve the per-repo pool from the registry by looking up
//     the repository's `database_name` on the system pool.
//  2. Insert a 'fetching' row using the URL's hash for the
//     "kind" field when the caller didn't supply one. The unique
//     constraint on (repository_id, url) makes the insert
//     idempotent: a re-enqueue of the same URL updates the
//     existing row instead of creating a duplicate.
//  3. Update the row to 'fetched' (with the body head-prefix)
//     or 'failed' (with the error message).
//
// Returns the source row's UUID so the caller can put it on
// the job result. Errors are non-fatal: the job is still
// considered successful from River's perspective; the caller
// logs and moves on.
func (w *RetrieveSourceWorker) persistSource(
	ctx context.Context,
	repoIDStr, url string,
	resource fetch.Resource,
	content fetch.ResolvedContent,
	fetchErr error,
	callerPublishedAt *time.Time,
) (string, error) {
	repoID := pgtype.UUID{}
	if err := repoID.Scan(repoIDStr); err != nil {
		return "", fmt.Errorf("invalid repository id %q: %w", repoIDStr, err)
	}

	dbName, err := w.systemQueries.GetRepositoryDatabaseName(ctx, repoID)
	if err != nil {
		return "", fmt.Errorf("resolving repository database: %w", err)
	}

	pool := w.registry.Get(dbName)
	queries := store.New(pool.Pool)

	// Best-effort insert. A unique-violation means the row
	// already exists from a previous run; we fall through to
	// the update. Any other error is returned.
	id := pgtype.UUID{}
	if err := id.Scan(uuid.New().String()); err != nil {
		return "", fmt.Errorf("generating source id: %w", err)
	}

	kind := string(resource.Type)
	if kind == "" {
		kind = "url"
	}

	// DOI is optional. The classifier populates it from a
	// doi.org URL or a bare "10.…" string; callers that
	// forward a search result's DOI would set it on the
	// resource before enqueuing. When both are empty we
	// persist NULL so the column stays clean for non-DOI
	// sources (homepages, datasets, generic PDFs).
	var doi *string
	if resource.DOI != "" {
		d := resource.DOI
		doi = &d
	}

	// publishedAt is the date the caller (or the search
	// result they clicked on) already knows. The worker
	// forwards it through the persistence call; when the
	// row is brand-new, CreateSource cannot carry it
	// (the SQL is a single statement and we keep the
	// column out of the hot create path), so the worker
	// writes it with UpdateSourcePublishedAt right
	// after a successful insert. When the row already
	// exists, the unique-violation fallback does the
	// same backfill. Conversion to pgtype.Date lives
	// in the helper below; the optional pointer is
	// what lets the column stay NULL for sources with
	// no date.
	publishedAt := toPgDate(callerPublishedAt)

	_, insertErr := queries.CreateSource(ctx, store.CreateSourceParams{
		ID:           id,
		RepositoryID: repoID,
		Url:          url,
		Kind:         kind,
		Status:       "fetching",
		Doi:          doi,
	})
	if insertErr != nil {
		// Look up the existing row's id so the update path
		// can target it. The (repository_id, url) UNIQUE
		// constraint guarantees at most one row, so the
		// focused GetSourceByRepoAndURL query is cheaper and
		// simpler than the previous "list every source in the
		// repo + filter in Go" approach.
		existing, listErr := queries.GetSourceByRepoAndURL(ctx, store.GetSourceByRepoAndURLParams{
			RepositoryID: repoID,
			Url:          url,
		})
		if listErr != nil {
			return "", fmt.Errorf("create or lookup source: create=%v lookup=%w", insertErr, listErr)
		}
		id = existing.ID

		// Backfill the DOI on the pre-existing row. The
		// CreateSource path wrote it directly, so we only
		// need this for the unique-violation branch where
		// the row predates this migration or was inserted
		// without a DOI. We don't bother clearing a
		// non-empty DOI back to NULL when the new resource
		// doesn't have one; that lets an operator
		// re-classify without losing the discovery.
		if doi != nil {
			if _, err := queries.UpdateSourceDoi(ctx, store.UpdateSourceDoiParams{
				ID:  id,
				Doi: doi,
			}); err != nil {
				log.Printf("retrieve_source: backfilling DOI on existing row failed: %v", err)
			}
		}

		// Backfill the publication date on the
		// pre-existing row, same shape as the DOI
		// backfill above. UpdateSourcePublishedAt is
		// a no-op when the date is NULL, so the
		// common case of "no date" doesn't write.
		if publishedAt.Valid {
			if _, err := queries.UpdateSourcePublishedAt(ctx, store.UpdateSourcePublishedAtParams{
				ID:          id,
				PublishedAt: publishedAt,
			}); err != nil {
				log.Printf("retrieve_source: backfilling published_at on existing row failed: %v", err)
			}
		}
	} else {
		// Brand-new row. CreateSource doesn't carry the
		// publication date (it would force a wider INSERT
		// and force the date through every other
		// caller, most of which have nothing to put
		// there), so we apply it as a small follow-up
		// UPDATE. UpdateSourcePublishedAt is a no-op
		// when publishedAt is invalid, so this is one
		// query in the common "no date" case rather
		// than a conditional branch.
		if publishedAt.Valid {
			if _, err := queries.UpdateSourcePublishedAt(ctx, store.UpdateSourcePublishedAtParams{
				ID:          id,
				PublishedAt: publishedAt,
			}); err != nil {
				log.Printf("retrieve_source: writing published_at on new row failed: %v", err)
			}
		}
	}

	// Mark the row as in-flight. Even when the row was just
	// created, flipping it to 'fetching' is cheap and keeps
	// the status-machine uniform (a future operator can
	// tell the difference between a row that was inserted
	// and a row that was actually attempted by looking at
	// the updated_at / fetched_at pair).
	if _, err := queries.MarkSourceFetching(ctx, id); err != nil {
		return "", fmt.Errorf("marking source fetching: %w", err)
	}

	if fetchErr != nil {
		errMsg := fetchErr.Error()
		_, err := queries.MarkSourceFailed(ctx, store.MarkSourceFailedParams{
			ID:    id,
			Error: &errMsg,
		})
		if err != nil {
			return "", fmt.Errorf("marking source failed: %w", err)
		}
		// Persist the audit trail so the UI can show
		// which tiers were tried and why they failed.
		persistFetchAttempts(ctx, queries, id, content.Attempts)
		// Persist the OA status (e.g. "closed") so the UI
		// can show users why the article is incomplete.
		persistOAStatus(ctx, queries, id, content.OAStatus)
		// Mark the parse as failed too. The parsed
		// fields are nullable; we leave them NULL and
		// set parse_status='failed' so the UI hides
		// the parsed view rather than showing stale
		// data from a previous attempt. published_at
		// is passed as a zero pgtype.Date so the
		// COALESCE in MarkSourceParsed preserves any
		// value the caller (or a previous parse) had
		// already set; the failure is about the parse,
		// not about the date.
		if _, err := queries.MarkSourceParsed(ctx, store.MarkSourceParsedParams{
			ID:          id,
			ParseStatus: strPtr("failed"),
		}); err != nil {
			log.Printf("retrieve_source: marking source parse failed: %v", err)
		}
		return uuidFromPgtype(id), nil
	}

	// Store a head-prefix of the body so a future UI can
	// render a preview without re-fetching. content.Body is
	// already a []byte; we copy at most
	// contentPreviewBytes of it. The column is BYTEA in the
	// database (since migration 0009) so it can carry
	// non-UTF-8 payloads like PDF or image bytes verbatim.
	preview := make([]byte, 0, min(len(content.Body), contentPreviewBytes+len("\n... [truncated]")))
	preview = append(preview, content.Body...)
	if len(preview) > contentPreviewBytes {
		preview = append(preview[:contentPreviewBytes], []byte("\n... [truncated]")...)
	}
	contentCopy := preview

	_, err = queries.MarkSourceFetched(ctx, store.MarkSourceFetchedParams{
		ID:      id,
		Content: contentCopy,
	})
	if err != nil {
		return "", fmt.Errorf("marking source fetched: %w", err)
	}

	// Persist the audit trail so the UI can show which
	// tier fetched the content and how long each attempt
	// took. Best-effort: persistFetchAttempts logs and
	// swallows errors.
	persistFetchAttempts(ctx, queries, id, content.Attempts)
	// Persist the OA status (e.g. "green", "closed") so
	// the UI can show users whether the article is open
	// access or paywalled. Best-effort.
	persistOAStatus(ctx, queries, id, content.OAStatus)

	// Store the full PDF body to the storage backend so the
	// /sources/{sourceID}/body endpoint can serve the original
	// document later (the row's `content` column only carries a
	// 32KB head-prefix). HTML / text bodies are NOT stored here
	// — text content is fully covered by the parsed text + the
	// DB preview, and storing HTML would balloon the storage
	// volume without adding capability. Only PDFs (the case
	// where re-fetching the original is expensive or impossible)
	// are persisted.
	//
	// Best-effort: a storage failure is logged but does not fail
	// the job. The row stays usable (text facts, image metadata)
	// without a downloadable body.
	if w.storage != nil && isPDFContentType(content.ContentType) && len(content.Body) > 0 {
		repoIDStr := uuidFromPgtype(repoID)
		srcIDStr := uuidFromPgtype(id)
		key := storageKeyForSourceBody(repoIDStr, srcIDStr)
		ref, storeErr := w.storage.Store(ctx, key, "application/pdf", content.Body)
		if storeErr != nil {
			log.Printf("retrieve_source: storing source body for %s failed: %v", srcIDStr, storeErr)
		} else {
			ct := "application/pdf"
			lp := key
			if _, err := queries.MarkSourceBodyStored(ctx, store.MarkSourceBodyStoredParams{
				ID:          id,
				StorageKey:  &ref.Key,
				ContentType: &ct,
				LocalPath:   &lp,
			}); err != nil {
				log.Printf("retrieve_source: marking source body stored for %s failed: %v", srcIDStr, err)
			}
		}
	}

	// Persist the parsed content + image list. The
	// parser already ran inside the fetch strategy
	// (or returned empty Parsed when no parser claimed
	// the response type). Either way the UI relies on
	// a non-NULL parse_status, so we always call
	// MarkSourceParsed even when there's nothing to
	// store.
	if err := w.persistParsedContent(ctx, queries, repoID, id, content); err != nil {
		log.Printf("retrieve_source: persisting parsed content failed: %v", err)
	}

	return uuidFromPgtype(id), nil
}

// uuidFromPgtype renders a pgtype.UUID as a string. The pgtype
// type stores the bytes directly; we convert to the canonical
// 8-4-4-4-12 hyphenated form so it matches what
// /api/v1/repositories/{repoID}/sources returns to the UI.
func uuidFromPgtype(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	b := id.Bytes
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// pgtypeToUUID converts a pgtype.UUID to a google/uuid.UUID.
func pgtypeToUUID(id pgtype.UUID) uuid.UUID {
	u, _ := uuid.FromBytes(id.Bytes[:])
	return u
}

// persistFetchAttempts marshals the strategy's audit trail
// (one entry per provider tried, in chain order) to JSON
// and writes it to the source row's fetch_attempts column.
// The write is best-effort: a marshalling or DB error is
// logged and swallowed so a failing audit write never
// breaks a successful fetch. The column is nullable; pass
// an empty slice to record "no attempts" (the strategy
// returns at least one attempt in every code path today).
func persistFetchAttempts(ctx context.Context, queries *store.Queries, id pgtype.UUID, attempts []fetch.FetchAttempt) {
	if len(attempts) == 0 {
		return
	}
	b, err := json.Marshal(attempts)
	if err != nil {
		log.Printf("retrieve_source: marshalling fetch_attempts: %v", err)
		return
	}
	if _, err := queries.MarkSourceFetchAttempts(ctx, store.MarkSourceFetchAttemptsParams{
		ID:            id,
		FetchAttempts: b,
	}); err != nil {
		log.Printf("retrieve_source: writing fetch_attempts: %v", err)
	}
}

// persistOAStatus writes the open-access status (reported by
// Unpaywall during the fetch strategy run) to the source row.
// The status is carried on ResolvedContent.OAStatus by the
// strategy, even when Unpaywall fell through to a different
// tier. Best-effort: errors are logged and swallowed.
func persistOAStatus(ctx context.Context, queries *store.Queries, id pgtype.UUID, oaStatus string) {
	if oaStatus == "" {
		return
	}
	s := oaStatus
	if _, err := queries.MarkSourceOAStatus(ctx, store.MarkSourceOAStatusParams{
		ID:       id,
		OaStatus: &s,
	}); err != nil {
		log.Printf("retrieve_source: writing oa_status: %v", err)
	}
}

// strPtr is a small helper for the *string params sqlc
// generates. Returning the address of a local variable
// would not compile in Go (the local escapes to the heap
// only via &, which we do here) but a helper keeps the
// call sites readable.
func strPtr(s string) *string { return &s }

// int32Ptr is the same idea for pgtype-friendly int32
// pointers.
func int32Ptr(v int32) *int32 { return &v }

// toPgDate converts a *time.Time (the wire shape used by
// the search-result and HTTP layers) into a pgtype.Date
// (the type sqlc generated for the published_at column).
// Returns a zero-value pgtype.Date (Valid=false) when the
// pointer is nil or points at the zero time, so the
// downstream UPDATE knows to skip the column. The time
// component is dropped on conversion: Postgres DATE has
// no sub-day precision, and every upstream we read from
// (OpenAlex publication_date, trafilatura / htmldate) is
// day-precision anyway.
func toPgDate(t *time.Time) pgtype.Date {
	if t == nil || t.IsZero() {
		return pgtype.Date{}
	}
	return pgtype.Date{Time: *t, Valid: true}
}

// persistParsedContent writes the ParsedDoc the fetch
// strategy produced onto the source row and into
// source_images. The strategy is the single place where
// parsing happens; the worker is the single place where
// parsed data lands in the database.
//
// Behavioural rules:
//
//   - Always call MarkSourceParsed, even when the parser
//     produced nothing, so parse_status is non-NULL. NULL
//     parse_status means "we have not tried to parse this
//     yet" (a row that was created but never fetched);
//     'ok' / 'unsupported' / 'failed' mean we tried.
//
//   - Always call ClearSourceImages before re-inserting.
//     A re-parse can drop images (a previous image
//     removed from the page, a PDF shortened), and the
//     row is the source of truth — stale references are
//     a real bug.
//
//   - Image insertion is best-effort: a single failing
//     image row must not fail the whole job. We log and
//     continue so a corrupt PNG does not block a
//     successful fetch.
func (w *RetrieveSourceWorker) persistParsedContent(
	ctx context.Context,
	queries *store.Queries,
	repoID pgtype.UUID,
	id pgtype.UUID,
	content fetch.ResolvedContent,
) error {
	parsed := content.Parsed

	// Decide parse_status. 'ok' when we got any field
	// back; 'unsupported' when the parser returned
	// nothing (no parser claimed the source type);
	// 'failed' is set by the MarkSourceFailed branch
	// above and does not flow through this function.
	status := "ok"
	if parsed.Title == "" && parsed.Text == "" && parsed.HTML == "" &&
		parsed.Markdown == "" &&
		len(parsed.Images) == 0 && len(parsed.PageImages) == 0 {
		status = "unsupported"
	}

	// sqlc *string params: nil = NULL column. We pass
	// a pointer to the empty string when the field is
	// non-NULL but blank so the UI distinguishes
	// "present, empty" from "absent". For 'ok' rows we
	// store everything the parser gave us; for
	// 'unsupported' we store nothing and let the
	// columns stay NULL.
	var title, text, html, markdown, author, sitename, language *string
	if status == "ok" {
		if parsed.Title != "" {
			title = strPtr(parsed.Title)
		}
		if parsed.Text != "" {
			text = strPtr(parsed.Text)
		}
		if parsed.HTML != "" {
			html = strPtr(parsed.HTML)
		}
		if parsed.Markdown != "" {
			markdown = strPtr(parsed.Markdown)
		}
		if parsed.Author != "" {
			author = strPtr(parsed.Author)
		}
		if parsed.Sitename != "" {
			sitename = strPtr(parsed.Sitename)
		}
		if parsed.Language != "" {
			language = strPtr(parsed.Language)
		}
	}

	// Surface the publication date the parser
	// recovered (trafilatura / htmldate). The
	// MarkSourceParsed query uses COALESCE on this
	// column, so a nil/invalid pgtype.Date is a no-op
	// — a value the search-result click-through path
	// or a previous parse set is preserved. The
	// "earliest known date wins" semantics mirrors
	// how DOI is backfilled on the same row: a
	// better-informed re-parse or a follow-up job
	// can refine the value, but a less-informed one
	// (or a re-parse after a content fix that doesn't
	// change the date) must not erase it.
	publishedAt := toPgDate(parsed.PublishedAt)

	if _, err := queries.MarkSourceParsed(ctx, store.MarkSourceParsedParams{
		ID:             id,
		ParsedTitle:    title,
		ParsedText:     text,
		ParsedHtml:     html,
		ParsedMarkdown: markdown,
		ParsedAuthor:   author,
		ParsedSitename: sitename,
		ParsedLanguage: language,
		PublishedAt:    publishedAt,
		ParseStatus:    strPtr(status),
	}); err != nil {
		return fmt.Errorf("marking source parsed: %w", err)
	}

	// Persist the deterministic global sentence array for this
	// source. The sentence splitter runs on the same field the
	// decomposition worker will chunk (markdown preferred, text
	// fallback) so the stored offsets are the stable contract that
	// fact_references.sentence_index keys into. Segmenting here
	// (not in decomposition) keeps the slow decomposition worker
	// simpler and guarantees sentences don't change during a
	// re-decomposition — the offsets are written once at parse
	// time and reused by every subsequent extraction pass.
	if offsets := buildSentenceOffsets(markdown, text); offsets != nil {
		if err := queries.SetSentenceOffsets(ctx, store.SetSentenceOffsetsParams{
			ID:              id,
			SentenceOffsets: offsets,
		}); err != nil {
			return fmt.Errorf("setting sentence offsets: %w", err)
		}
	}

	// Replace the image set wholesale. A re-parse
	// returns the full new list, so the simplest
	// correct semantics is delete-then-insert. We
	// could diff against the existing rows to avoid
	// a few writes, but the savings are small and
	// the diff is easy to get wrong (image URL
	// changes, page reorder, etc.).
	if err := queries.ClearSourceImages(ctx, id); err != nil {
		return fmt.Errorf("clearing source images: %w", err)
	}
	if err := w.persistImages(ctx, queries, repoID, id, parsed); err != nil {
		return fmt.Errorf("persisting source images: %w", err)
	}
	return nil
}

// persistImages writes inline images and PDF page renders, and
// (when a storage backend is configured) persists their bytes to
// the storage backend so the serving endpoint can stream them back
// without re-fetching the remote URL.
//
// Inline images: the parser surfaced the absolute URL; the worker
// inserts the row, then — when storage is configured — fetches the
// bytes via the fetch provider's FetchImageBytes, stores them under
// `repositories/{repoID}/sources/{sourceID}/images/{imageID}.{ext}`,
// and updates the row with the storage_key / content_type /
// mirrored_at. The original `url` is preserved on the row so the
// frontend can fall back to the remote URL when storage_key is NULL
// (mirror failed, or storage disabled).
//
// Page renders: the parser produced in-memory PNG bytes (one per
// PDF page). The worker inserts the row with the byte *length* in
// `bytes` (the column's historical shape), then — when storage is
// configured — stores the payload directly (no re-fetch needed) and
// updates the row the same way. A page render has no remote URL, so
// when storage_key is NULL the frontend shows a placeholder card.
//
// All storage work is best-effort: a single failing image must not
// fail the whole job. We log and continue so a corrupt PNG does
// not block a successful fetch.
func (w *RetrieveSourceWorker) persistImages(
	ctx context.Context,
	queries *store.Queries,
	repoID pgtype.UUID,
	id pgtype.UUID,
	parsed content_parsing.ParsedDoc,
) error {
	repoIDStr := uuidFromPgtype(repoID)
	srcIDStr := uuidFromPgtype(id)

	for i, img := range parsed.Images {
		url := img.URL
		if url == "" {
			continue
		}
		alt := img.Alt
		var altPtr *string
		if alt != "" {
			altPtr = &alt
		}
		row, err := queries.AddSourceImage(ctx, store.AddSourceImageParams{
			SourceID: id,
			Kind:     "inline",
			Position: int32(i),
			Url:      &url,
			AltText:  altPtr,
		})
		if err != nil {
			// CHECK constraint violation here would
			// mean the parser produced a URL that
			// failed the url-not-empty check after
			// absolutization. Log and continue; the
			// image is non-critical.
			log.Printf("retrieve_source: adding inline image %d failed: %v", i, err)
			continue
		}

		// Mirror the image bytes to storage. Best-effort: on
		// any error the row stays with NULL storage_key and the
		// frontend falls back to the remote URL.
		if w.storage == nil {
			continue
		}
		if err := w.mirrorInlineImage(ctx, queries, row, repoIDStr, srcIDStr, url); err != nil {
			log.Printf("retrieve_source: mirroring inline image %s failed: %v", uuidFromPgtype(row.ID), err)
		}
	}

	for i, page := range parsed.PageImages {
		if len(page.Bytes) == 0 {
			continue
		}
		width, height := pngDimensions(page.Bytes)
		bytes := int32(len(page.Bytes))
		pageNum := int32(page.Page)
		row, err := queries.AddSourceImage(ctx, store.AddSourceImageParams{
			SourceID:   id,
			Kind:       "page",
			PageNumber: &pageNum,
			Position:   int32(i),
			Width:      int32PtrNillable(width),
			Height:     int32PtrNillable(height),
			Bytes:      &bytes,
		})
		if err != nil {
			log.Printf("retrieve_source: adding page image %d failed: %v", i, err)
			continue
		}

		// Store the page-render bytes directly (no re-fetch
		// needed — they're already in memory). Best-effort: on
		// any error the row stays with NULL storage_key and the
		// frontend shows a placeholder.
		if w.storage == nil {
			continue
		}
		if err := w.mirrorPageRender(ctx, queries, row, repoIDStr, srcIDStr, page.Bytes); err != nil {
			log.Printf("retrieve_source: storing page render %s failed: %v", uuidFromPgtype(row.ID), err)
		}
	}
	return nil
}

// mirrorInlineImage fetches the image bytes via the fetch provider's
// FetchImageBytes helper, stores them under the canonical image key,
// and updates the row with the storage metadata. The fetch provider
// caps the body at maxImageBytes (5 MB by default) and sniffs the
// content-type from headers + URL extension.
func (w *RetrieveSourceWorker) mirrorInlineImage(
	ctx context.Context,
	queries *store.Queries,
	row store.OktRepositorySourceImage,
	repoIDStr, srcIDStr, url string,
) error {
	// We need a FetchResolutionProvider to call FetchImageBytes.
	// The worker holds a *fetch.FetchStrategy; pull the first
	// provider that implements *FetchResolutionProvider. The
	// strategy iterates providers in priority order; the
	// catch-all HTTP fetch is always the last one, so we walk
	// from the back to find it without naming the type.
	fp := w.fetchImageProvider()
	if fp == nil {
		return fmt.Errorf("no fetch provider available for image download")
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body, contentType, err := fp.FetchImageBytes(fetchCtx, url, maxImageDownloadBytes)
	if err != nil {
		return fmt.Errorf("fetching image bytes: %w", err)
	}
	key := storageKeyForImage(repoIDStr, srcIDStr, uuidFromPgtype(row.ID), contentType)
	ref, err := w.storage.Store(ctx, key, contentType, body)
	if err != nil {
		return fmt.Errorf("storing image: %w", err)
	}
	lp := ref.Key
	ct := contentType
	if _, err := queries.MarkSourceImageStored(ctx, store.MarkSourceImageStoredParams{
		ID:          row.ID,
		StorageKey:  &ref.Key,
		ContentType: &ct,
		LocalPath:   &lp,
	}); err != nil {
		return fmt.Errorf("marking image stored: %w", err)
	}
	return nil
}

// mirrorPageRender stores the in-memory PNG bytes of a PDF page
// render under the canonical image key and updates the row with the
// storage metadata. No fetch is needed — the bytes are already in
// hand from the parser.
func (w *RetrieveSourceWorker) mirrorPageRender(
	ctx context.Context,
	queries *store.Queries,
	row store.OktRepositorySourceImage,
	repoIDStr, srcIDStr string,
	body []byte,
) error {
	contentType := "image/png"
	key := storageKeyForImage(repoIDStr, srcIDStr, uuidFromPgtype(row.ID), contentType)
	ref, err := w.storage.Store(ctx, key, contentType, body)
	if err != nil {
		return fmt.Errorf("storing page render: %w", err)
	}
	lp := ref.Key
	ct := contentType
	if _, err := queries.MarkSourceImageStored(ctx, store.MarkSourceImageStoredParams{
		ID:          row.ID,
		StorageKey:  &ref.Key,
		ContentType: &ct,
		LocalPath:   &lp,
	}); err != nil {
		return fmt.Errorf("marking page render stored: %w", err)
	}
	return nil
}

// fetchImageProvider returns the *fetch.FetchResolutionProvider on
// the strategy, if any. The strategy's providers are a mix of
// types (Unpaywall, fetch HTTP, future); only the HTTP fetch
// provider implements FetchImageBytes. We walk the list and return
// the first match.
func (w *RetrieveSourceWorker) fetchImageProvider() *fetch.FetchResolutionProvider {
	if w.fetchStrategy == nil {
		return nil
	}
	for _, p := range w.fetchStrategy.Providers() {
		if fp, ok := p.(*fetch.FetchResolutionProvider); ok {
			return fp
		}
	}
	return nil
}

// maxImageDownloadBytes caps the body of an inline image download.
// Mirrors the image-extraction cap (5 MB by default in
// config.default.yaml); a single huge image shouldn't tank the
// worker.
const maxImageDownloadBytes int64 = 5 * 1024 * 1024

// storageKeyForImage builds the canonical storage key for a source
// image: `repositories/{repoID}/sources/{sourceID}/images/{imageID}.{ext}`.
// The extension is derived from the sniffed content-type so the
// filesystem backend lays files out with the right suffix and the
// serving endpoint can sniff a default Content-Type from the path.
func storageKeyForImage(repoID, srcID, imageID, contentType string) string {
	ext := extForContentType(contentType)
	if ext != "" {
		ext = "." + ext
	}
	return fmt.Sprintf("repositories/%s/sources/%s/images/%s%s", repoID, srcID, imageID, ext)
}

// storageKeyForSourceBody builds the canonical storage key for a
// full source body (today: PDFs only):
// `repositories/{repoID}/sources/{sourceID}/body.pdf`.
func storageKeyForSourceBody(repoID, srcID string) string {
	return fmt.Sprintf("repositories/%s/sources/%s/body.pdf", repoID, srcID)
}

// extForContentType maps a sniffed image/PDF content-type to a file
// extension. Returns "" for unknown types so the key has no suffix
// (the serving endpoint still serves with the DB-recorded
// Content-Type).
func extForContentType(ct string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0])) {
	case "image/png":
		return "png"
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	case "image/svg+xml":
		return "svg"
	case "image/bmp":
		return "bmp"
	case "application/pdf":
		return "pdf"
	}
	return ""
}

// isPDFContentType returns true when the content-type indicates a
// PDF payload. Used to gate full-body storage (only PDFs are stored
// today; HTML / text bodies stay as the DB preview).
func isPDFContentType(ct string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "application/pdf")
}

// int32PtrNillable returns nil for the zero dimension
// (which the image package reports for unrecognised
// formats) so the column is NULL rather than 0. A page
// render whose dimensions we couldn't decode is still
// useful — we just don't claim a width or height.
func int32PtrNillable(v int) *int32 {
	if v <= 0 {
		return nil
	}
	return int32Ptr(int32(v))
}

// pngDimensions reads the width and height out of a PNG
// or JPEG byte slice. Returns (0, 0) when the bytes are
// not a recognised image format. The stdlib image
// decoders are imported anonymously for their side
// effects (image.Decode can identify them).
func pngDimensions(b []byte) (int, int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// Format a brief string for log output. Kept here so other packages
// (e.g. a future HTTP handler returning job status) can render a job
// the same way.
func (r RetrieveSourceResult) String() string {
	if r.Fetched {
		return fmt.Sprintf("classified_as=%s fetched=%d bytes status=%d final_url=%s",
			r.ClassifiedAs, r.Bytes, r.StatusCode, r.FinalURL)
	}
	return fmt.Sprintf("classified_as=%s fetched=false", r.ClassifiedAs)
}

// buildSentenceOffsets produces the flat
// [start0, end0, start1, end1, ...] rune-offset array for the source
// text the decomposition worker will chunk. Markdown is preferred
// over plain text to match the decomposition worker's field
// selection; the offsets are the stable contract that
// fact_references.sentence_index keys into. Returns nil when the
// source has no parseable text so the column stays NULL (the
// decomposition worker will skip sentence labeling for such
// sources).
func buildSentenceOffsets(markdown, text *string) []int32 {
	var src string
	switch {
	case markdown != nil && *markdown != "":
		src = *markdown
	case text != nil && *text != "":
		src = *text
	default:
		return nil
	}
	sents := decomposition.SegmentSentences(src)
	if len(sents) == 0 {
		return nil
	}
	offsets := make([]int32, 0, len(sents)*2)
	for _, s := range sents {
		offsets = append(offsets, int32(s.StartRune), int32(s.EndRune))
	}
	return offsets
}
