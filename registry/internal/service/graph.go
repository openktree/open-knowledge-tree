package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"

	"github.com/openktree/knowledge-registry/internal/model"
	"github.com/openktree/knowledge-registry/internal/storage"
)

// graphSchemaVersion is the bundle schema version the registry stamps
// on every graph it indexes. The client (OKT export task) sets it on
// the bundle; the registry echoes the stored value back on read.
const graphSchemaVersion = 1

// GraphKey is the S3 object key for a graph bundle. The bundle is
// gzipped JSON (see internal/providers/graph/gzip.go on the OKT side),
// so the key carries the .json.gz suffix to make the content-encoding
// obvious to operators browsing the bucket.
func GraphKey(graphID string) string {
	return fmt.Sprintf("graphs/%s.json.gz", graphID)
}

// PushGraph indexes a shared knowledge graph bundle. The bundle bytes
// (gzipped JSON) are written to S3 at `graphs/{id}.json.gz`; the
// metadata row is upserted into the graphs table. Dedup: when the
// caller leaves meta.ID empty, the service searches for an existing
// graph with the same (name, sha256) and reuses its id; otherwise a
// fresh uuid is generated. Reusing the id means a re-push of the same
// graph overwrites the same S3 object and refreshes the metadata row
// (counts, tags, description) rather than creating a duplicate.
//
// Mirrors PushSource's fire-and-forget S3 write: the metadata row is
// the source of truth for search, and the S3 object is overwrite-safe
// (the next push writes the same key). A pull in the ~50–200ms window
// before S3 catches up gets a 404, which the OKT import path handles
// gracefully (it logs and falls back to the registry's PullGraph
// endpoint, which re-reads from S3).
func (r *Registry) PushGraph(ctx context.Context, meta *model.GraphMeta, bundle []byte) (*model.GraphPushResult, error) {
	if err := acquire(ctx, r.pushSem); err != nil {
		return nil, fmt.Errorf("waiting for push concurrency slot: %w", err)
	}
	defer release(r.pushSem)

	graphID := meta.ID
	if graphID == "" {
		existing, err := r.findExistingGraph(ctx, meta.Name, meta.SHA256)
		if err != nil {
			return nil, fmt.Errorf("searching for existing graph: %w", err)
		}
		if existing != nil {
			graphID = existing.ID
		} else {
			graphID = uuid.New().String()
		}
	}
	meta.ID = graphID
	if meta.SchemaVersion == 0 {
		meta.SchemaVersion = graphSchemaVersion
	}

	s3Key := GraphKey(graphID)
	meta.S3Key = s3Key

	// Fire-and-forget: the S3 object is an overwrite-safe blob (the
	// next push writes the same key). A failure here logs and the
	// object is stale until the next push; the metadata DB is already
	// consistent so a search will find the graph. A pull in the
	// ~50–200ms window before S3 catches up gets a 404, which the OKT
	// import path handles gracefully.
	go func() {
		if err := r.storage.Store(ctx, s3Key, bundle, "application/gzip"); err != nil {
			log.Printf("registry: async Store for graph %s: %v", graphID, err)
		}
	}()

	now := time.Now().UTC()
	meta.CreatedAt = now
	meta.UpdatedAt = now
	if err := r.store.IndexGraph(ctx, meta); err != nil {
		return nil, fmt.Errorf("indexing graph: %w", err)
	}

	// Determine whether this push created a new row vs refreshed an
	// existing one. We rely on the caller's meta.ID: when it was empty
	// and findExistingGraph returned nil, it's a new graph; otherwise
	// it's a refresh of an existing id.
	created := meta.ID == graphID && graphID != ""
	return &model.GraphPushResult{GraphID: graphID, New: created}, nil
}

// findExistingGraph searches the store for a graph matching the given
// (name, sha256). Returns nil when no match is found. The sha256 is
// the stronger signal (a re-push of the same bundle has the same
// hash); the name is a secondary check so two different bundles with
// the same name don't collide.
func (r *Registry) findExistingGraph(ctx context.Context, name, sha256 string) (*model.GraphMeta, error) {
	if sha256 == "" {
		return nil, nil
	}
	// Linear scan over the graphs table; the registry's shared-graph
	// count is small (few per instance), so a full scan is cheaper
	// than maintaining a sha256 index for a dedup that fires once per
	// export. If the registry ever hosts thousands of graphs, add a
	// sha256 index + a focused lookup.
	graphs, err := r.store.ListGraphs(ctx, 10000, 0)
	if err != nil {
		return nil, err
	}
	for i := range graphs {
		if graphs[i].SHA256 == sha256 && graphs[i].Name == name {
			return &graphs[i], nil
		}
	}
	return nil, nil
}

// PullGraph returns the metadata for a graph. The bundle bytes are
// NOT returned here (they can be large); callers fetch them via the
// presigned URL on the meta (PresignedDownloadURL) or via
// PullGraphBundle. Mirrors PullSource's metadata-only shape (the
// decompositions list is annotated with presigned URLs, not inlined).
func (r *Registry) PullGraph(ctx context.Context, graphID string) (*model.GraphMeta, error) {
	if err := acquire(ctx, r.pullSem); err != nil {
		return nil, fmt.Errorf("waiting for pull concurrency slot: %w", err)
	}
	defer release(r.pullSem)

	meta, err := r.store.GetGraph(ctx, graphID)
	if err != nil {
		return nil, fmt.Errorf("reading graph %s: %w", graphID, err)
	}
	// Annotate with a presigned download URL so the client can fetch
	// the gzipped bundle straight from object storage without proxying
	// through the registry service.
	if meta.S3Key != "" {
		if pu, err := r.storage.PresignedURL(ctx, meta.S3Key, r.presignTTL); err == nil {
			meta.PresignedURL = pu
		} else if !errors.Is(err, storage.ErrPresignDisabled) {
			log.Printf("registry: presigning graph %s: %v", graphID, err)
		}
	}
	return meta, nil
}

// PullGraphBundle streams the raw gzipped bundle bytes for a graph.
// Used by the OKT import path's fallback when the presigned URL fast
// path is unavailable (filesystem backend, dev mode). The caller is
// responsible for gunzipping + json.Unmarshal.
func (r *Registry) PullGraphBundle(ctx context.Context, graphID string) ([]byte, string, error) {
	if err := acquire(ctx, r.pullSem); err != nil {
		return nil, "", fmt.Errorf("waiting for pull concurrency slot: %w", err)
	}
	defer release(r.pullSem)

	meta, err := r.store.GetGraph(ctx, graphID)
	if err != nil {
		return nil, "", fmt.Errorf("reading graph %s: %w", graphID, err)
	}
	data, contentType, err := r.storage.ReadAll(ctx, meta.S3Key)
	if err != nil {
		return nil, "", fmt.Errorf("reading graph bundle %s: %w", graphID, err)
	}
	return data, contentType, nil
}

// ListGraphs returns a paginated list of graph metadata, optionally
// filtered by a free-text query (name + description) or an exact tag.
// The query and tag filters are mutually exclusive: when both are
// non-empty, the query wins (the caller decides which to pass). The
// total is the unfiltered-or-filtered count matching the same filter.
func (r *Registry) ListGraphs(ctx context.Context, q model.GraphSearchQuery) (*model.GraphListResult, error) {
	limit := q.Limit
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	var (
		graphs []model.GraphMeta
		total  int
		err    error
	)
	switch {
	case q.Tag != "":
		graphs, err = r.store.SearchGraphsByTag(ctx, q.Tag, limit, q.Offset)
		if err != nil {
			return nil, err
		}
		total, err = r.store.CountGraphsByTag(ctx, q.Tag)
	case q.Query != "":
		graphs, err = r.store.SearchGraphsByText(ctx, q.Query, limit, q.Offset)
		if err != nil {
			return nil, err
		}
		total, err = r.store.CountGraphsByText(ctx, q.Query)
	default:
		graphs, err = r.store.ListGraphs(ctx, limit, q.Offset)
		if err != nil {
			return nil, err
		}
		total, err = r.store.CountGraphs(ctx)
	}
	if err != nil {
		return nil, err
	}
	// Annotate each graph with a presigned download URL so the client
	// can fetch the bundle without a second round-trip.
	for i := range graphs {
		if graphs[i].S3Key == "" {
			continue
		}
		if pu, err := r.storage.PresignedURL(ctx, graphs[i].S3Key, r.presignTTL); err == nil {
			graphs[i].PresignedURL = pu
		} else if !errors.Is(err, storage.ErrPresignDisabled) {
			log.Printf("registry: presigning graph %s (list): %v", graphs[i].ID, err)
		}
	}
	return &model.GraphListResult{Graphs: graphs, Total: total}, nil
}

// DeleteGraph removes a graph's metadata row + S3 object. The S3
// delete is best-effort (a missing object is fine); the metadata row
// delete is the source of truth. Mirrors the source-delete pattern
// (the registry doesn't expose source delete today, but the graph
// delete is owner/admin-gated at the HTTP layer).
func (r *Registry) DeleteGraph(ctx context.Context, graphID string) error {
	meta, err := r.store.GetGraph(ctx, graphID)
	if err != nil {
		return fmt.Errorf("reading graph %s: %w", graphID, err)
	}
	if err := r.store.DeleteGraph(ctx, graphID); err != nil {
		return fmt.Errorf("deleting graph metadata %s: %w", graphID, err)
	}
	if meta.S3Key != "" {
		if err := r.storage.Delete(ctx, meta.S3Key); err != nil && !errors.Is(err, storage.ErrNotFound) {
			log.Printf("registry: deleting graph bundle %s: %v", graphID, err)
		}
	}
	return nil
}

// StatsGraphs returns the total graph count for the registry dashboard.
func (r *Registry) StatsGraphs(ctx context.Context) (int, error) {
	return r.store.CountGraphs(ctx)
}

// MarshalGraphBundleJSON is a thin helper the HTTP handler uses to
// decode an inbound graph bundle from the request body. Kept here
// (next to PushGraph) so the bundle shape + schema version stay in
// one place. The actual GraphBundle type lives in the OKT
// providers/graph package; the registry treats the bundle as an
// opaque gzipped JSON blob, so this helper is just json.Unmarshal.
func MarshalGraphBundleJSON(data []byte, out interface{}) error {
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("unmarshaling graph bundle: %w", err)
	}
	return nil
}
