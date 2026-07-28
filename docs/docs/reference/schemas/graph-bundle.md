---
id: graph-bundle
sidebar_position: 2
title: Graph Bundle
---

# Graph Bundle

The **Graph Bundle** is the per-repository interchange format. It is the entire derived layer of one OKT repository — sources, facts, concepts, summaries, syntheses, investigations, reports, embeddings, and optionally the source bodies and images — serialized as one gzipped JSON document. Push a graph bundle to the Knowledge Registry and any other OKT instance can import it into a fresh repository in a single job: no re-fetch, no re-decomposition, no re-summarization, no re-synthesis, **zero LLM cost**.

Where the [Decomposition Package](/docs/reference/schemas/decomposition-package) shares one source's processing, the Graph Bundle shares a whole curated, researched repository. It is the format that turns a knowledge graph into a portable, addressable artifact.

**Source of truth:** `backend/internal/providers/graph/bundle.go` — `GraphBundle`.

## Design goals

1. **Whole-repository in one file.** A consumer imports a complete knowledge graph — sources through syntheses, plus reports and investigations — with one streaming pass. No second import step, no missing pieces.
2. **UUID-remappable.** Every internal cross-reference uses bundle-local integer indices (`fact_idx`, `source_idx`, `concept_idx`, ...), not UUIDs. The importer remaps each index to a fresh local UUID on the fly, so a graph can be imported into a repository that already has data without UUID collisions.
3. **Canonical hash for dedup.** The bundle carries a `metadata.sha256` computed over a canonical form (with images, bodies, source images, source bodies, and embeddings zeroed out — they are derived from sources and don't affect graph identity). The registry dedups re-pushes of the same graph by `(name, sha256)`.
4. **Streaming-friendly.** The JSON field order is laid out so a streaming importer can resolve every forward reference as it arrives (images and bodies come before facts, which come before the summaries/syntheses that reference them). No buffering the whole bundle in memory.
5. **Optional embedded media.** Source PDFs and images can be embedded (base64 in JSON, gzipped) so the bundle is fully self-contained, or omitted so the bundle stays small and the consumer re-fetches from the original URLs.
6. **Promptset- and model-tagged.** Like the decomposition package, every fact/concept/link carries a `promptset_hash`, and the embeddings block carries its `model` + `dimensions`. A consumer can filter, audit, and decide on re-embedding independently per section.

## Top-level shape

```json
{
  "schema_version": 2,
  "metadata": { /* BundleMetadata */ },
  "sources": [ /* SourceRow[] */ ],
  "images": { "imgkey": { /* FileBytes */ } },
  "bodies": { "bodykey": { /* FileBytes */ } },
  "source_images": [ /* SourceImageRow[] */ ],
  "source_bodies": [ /* SourceBodyRef[] */ ],
  "facts": [ /* FactRow[] */ ],
  "fact_sources": [ /* FactSourceRow[] */ ],
  "concepts": [ /* ConceptRow[] */ ],
  "concept_aliases": [ /* ConceptAliasRow[] */ ],
  "fact_concepts": [ /* FactConceptRow[] */ ],
  "concept_summaries": [ /* SummaryRow[] */ ],
  "concept_syntheses": [ /* SynthesisRow[] */ ],
  "investigations": [ /* InvestigationRow[] */ ],
  "investigation_sources": [ /* InvestigationSourceRow[] */ ],
  "reports": [ /* ReportRow[] */ ],
  "report_annotations": [ /* ReportAnnotationRow[] */ ],
  "embeddings": { /* Embeddings — optional */ }
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `schema_version` | int | yes | Bundle format version. **Current: `2`**. The importer refuses bundles newer than it understands. |
| `metadata` | `BundleMetadata` | yes | Human-readable header the registry indexes for search and the UI displays without downloading the bundle. |
| `sources` | `SourceRow[]` | yes | The sources. Every other section references these by `idx`. |
| `images` | `map[string]FileBytes` | no | Embedded image bytes, keyed by image ref. Present only when the export opted into images and at least one image has local bytes. |
| `bodies` | `map[string]FileBytes` | no | Embedded source bodies (e.g. PDFs), keyed by body ref. Present only when the export opted into `include_bodies`. |
| `source_images` | `SourceImageRow[]` | yes | One row per `source_images` entry. `image_ref` keys into `images`. |
| `source_bodies` | `SourceBodyRef[]` | no | One entry per source with a stored body. `body_ref` keys into `bodies`. Absent when `include_bodies=false`. |
| `facts` | `FactRow[]` | yes | Atomic facts. |
| `fact_sources` | `FactSourceRow[]` | yes | Fact ↔ source junction. |
| `concepts` | `ConceptRow[]` | yes | Concept nodes. |
| `concept_aliases` | `ConceptAliasRow[]` | yes | Concept aliases. |
| `fact_concepts` | `FactConceptRow[]` | yes | Fact ↔ concept junction. |
| `concept_summaries` | `SummaryRow[]` | yes | Per-concept summary slices. |
| `concept_syntheses` | `SynthesisRow[]` | yes | One authoritative synthesis per concept group. |
| `investigations` | `InvestigationRow[]` | yes | Investigations (may be empty). |
| `investigation_sources` | `InvestigationSourceRow[]` | yes | Investigation ↔ source links. |
| `reports` | `ReportRow[]` | yes | Reports (may be empty). |
| `report_annotations` | `ReportAnnotationRow[]` | yes | Per-sentence fact annotations on reports. |
| `embeddings` | `Embeddings` | no | Qdrant vectors for facts and concepts. Omitted when the exporting repo had no embeddings. |

## `metadata`

```json
{
  "name": "Agroforestry Meta-Synthesis",
  "description": "...",
  "owner": "user@example.com",
  "tags": ["agroforestry", "research"],
  "okt_version": "1.4.0",
  "promptset_hashes": ["ab12..."],
  "embedding_model": "BAAI/bge-large-en-v1.5",
  "embedding_dimensions": 1024,
  "source_count": 42,
  "fact_count": 1287,
  "concept_count": 96,
  "summary_count": 134,
  "synthesis_count": 12,
  "report_count": 3,
  "investigation_count": 2,
  "sha256": "7e3f...canonical-hash",
  "exported_at": "2026-07-28T12:00:00Z"
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Human-readable graph name. Part of the registry dedup key `(name, sha256)`. |
| `description` | string | no | Free-text description. |
| `owner` | string | no | Exporting user/system. The registry overwrites this with the auth identity on push. |
| `tags` | string[] | no | Search/filter tags. |
| `okt_version` | string | no | OKT version that produced the bundle (audit). |
| `promptset_hashes` | string[] | no | All promptset hashes present in the bundle. A consumer can admit/deny the whole bundle by checking these against its accepted set. |
| `embedding_model` | string | no | The embedding model used for the `embeddings` block. |
| `embedding_dimensions` | int | no | Vector dimensionality. |
| `source_count` / `fact_count` / `concept_count` / `summary_count` / `synthesis_count` / `report_count` / `investigation_count` | int | yes | Section sizes. The registry echoes these on read so the browse UI can show graph size without downloading the bundle. |
| `sha256` | string | no | Canonical hash (see below). Empty on the export side until the builder's pass 1 fills it in; the registry indexes it for dedup. |
| `exported_at` | time (RFC3339) | yes | When the export ran. |

## Index-based cross-references

The bundle uses bundle-local integer indices instead of UUIDs for every internal link. The importer maintains an `idx → fresh local UUID` map per entity type and remaps as it inserts.

| Row | Index field | Referenced by |
|-----|-------------|---------------|
| `sources` | `idx` | `fact_sources.source_idx`, `investigation_sources.source_idx`, `source_images.source_idx`, `source_bodies.source_idx` |
| `facts` | `idx` | `fact_sources.fact_idx`, `fact_concepts.fact_idx`, `report_annotations.fact_idx`, `summary.covered_fact_idxs` |
| `concepts` | `idx` | `concept_aliases.concept_idx`, `fact_concepts.concept_idx`, `summary.concept_idx`, `synthesis.covered_concept_idxs` |
| `investigations` | `idx` | `investigation_sources.investigation_idx` |
| `reports` | `idx` | `report_annotations.report_idx`, child reports via `parent_idx` |

Sentinel `-1` (or omitted) means "none" for `parent_idx` and `source_image_idx`.

## Key sections

### `sources[]`

```json
{
  "idx": 0,
  "url": "https://example.com/paper",
  "doi": "10.1234/...",
  "kind": "pdf",
  "status": "processed",
  "parsed_title": "...",
  "parsed_text": "...",
  "parsed_markdown": "...",
  "published_at": "2025-03-01",
  "sha256": "1b2c...content-hash",
  "has_stored_body": true
}
```

`sha256` is computed by the builder from the parsed content — it is **not** a column on the `sources` table, but it lets the registry dedup re-pushes of the same graph and lets an importer detect that an incoming source matches an existing local source.

### `facts[]`

```json
{
  "idx": 17,
  "text": "Mycorrhizal fungi transfer phosphorus...",
  "fact_kind": "text",
  "image_url": "",
  "content_hash": "9a17...sha256",
  "promptset_hash": "ab12...",
  "status": "stable",
  "source_image_idx": -1
}
```

`content_hash` is the same dedup key as in the Decomposition Package (SHA-256 of normalized text). On import, a fact whose `content_hash` already exists in the local repo is merged into the existing row rather than duplicated. `source_image_idx` (when ≥ 0) identifies the `source_images` row whose image backs an image fact, so the importer can remap `image_url` to the locally stored copy.

### `concept_summaries[]` and `concept_syntheses[]`

Summaries carry `covered_fact_idxs` (remapped to fresh UUIDs on import). Syntheses carry `covered_summary_idxs`, `covered_concept_idxs`, and `embedded_image_idxs`. The markdown content uses the `[text](<fact:FACT_UUID>)` citation form, but on import the UUIDs are the bundle producer's — the importer rewrites them to the local UUIDs during the remap pass, so citations in the imported repository resolve correctly.

### `embeddings` (optional)

```json
{
  "model": "BAAI/bge-large-en-v1.5",
  "dimensions": 1024,
  "fact_vectors": { "17": [0.01, ...], "18": [0.02, ...] },
  "concept_vectors": { "3": [0.03, ...] }
}
```

Vectors are keyed by the stringified fact/concept **idx** (matching the bundle's index scheme), so the importer can look up the remapped local UUID and upsert. As with the decomposition package: if `model` matches the consumer's configured embedding model, vectors are upserted directly; otherwise the block is dropped and the importer enqueues local `embed_facts` / `embed_concepts` jobs to re-vectorize with its own model.

### `images` / `bodies` (optional, embedded media)

```json
{
  "content_type": "image/png",
  "data": "base64..."
}
```

`FileBytes` carries raw bytes (base64 in JSON, gzipped at the bundle level). `images` is keyed by the `image_ref` that `source_images[].image_ref` references; `bodies` is keyed by the `body_ref` that `source_bodies[].body_ref` references. When `include_images=false` / `include_bodies=false`, these maps and their ref columns are absent, and the importer re-fetches from the original URLs (or loses the body for `upload://` sources that have no remote URL).

## The canonical hash

`metadata.sha256` is computed over a **canonical form** of the bundle:

- `images`, `bodies`, `source_images`, `source_bodies`, and `embeddings` are zeroed out (they are all derived from the sources — including or excluding them doesn't change graph identity).
- The rest of the bundle is serialized in a stable field order.

This makes the hash stable across re-exports that toggle media embedding or re-embed with a different model — the same graph exported with and without bodies produces the same `sha256`, so the registry dedups them as the same graph. The exporter runs a two-pass build: pass 1 streams to `/dev/null` to compute the canonical sha, pass 2 streams to the temp file with `metadata.sha256` populated, then pushes the temp file. The bundle the registry stores always carries the populated hash.

## Versioning

`schema_version` is **`2`**. History:

- **v1** — initial. Required the importer to buffer the whole bundle in memory because images/bodies came after facts (forward reference).
- **v2** (current) — moves `images`/`bodies`/`source_images`/`source_bodies` before `facts` in field order, eliminating the forward reference and enabling streaming import. The canonical hash now also excludes `source_images` and `source_bodies`.

The importer:

- accepts bundles with `schema_version <=` its supported version, applying any defined upgrade path;
- rejects bundles with `schema_version >` its supported version with a clear error, never silently truncating.

A new optional field or section does **not** bump the version — `encoding/json` leaves absent sections as empty/nil, and consumers treat absent the same as the field's documented default.

## Producing a bundle

The OKT backend produces a graph bundle in the `export_graph` River worker (`backend/internal/taskmanager/tasks/export_graph.go`), enqueued by `POST /{repoID}/export-graph`. The worker:

1. Resolves the per-repo pool and the registry client.
2. Runs a two-pass streaming build via `graph.BundleBuilder` (pass 1 = hash only, pass 2 = write to temp file with the hash in metadata).
3. Streams the gzipped temp file to the registry's `PushGraphStream` — indexing metadata travels in `X-Graph-*` headers; the body is a pure byte pipe to the registry's object store, never parsed by the registry process.

Export options:

- **`include_bodies`** (default `false`) — embed source PDFs in the bundle. Off keeps the bundle small; the consumer re-fetches URLs.
- **`include_images`** (default `true`) — embed source images. On by default so image facts stay resolvable; off for minimal-size exports.

## Consuming a bundle

The OKT backend consumes a graph bundle in the `import_graph` River worker, enqueued by `POST /{repoID}/import-graph`. The worker streams the bundle from the registry and, in a single pass:

1. Inserts sources, building `sourceIdx → local source UUID`.
2. Inserts `source_images` (and writes embedded image bytes to local storage when present), building `sourceImageIdx → local image UUID`.
3. Inserts `source_bodies` (and writes embedded body bytes when present).
4. Inserts facts, deduping on `content_hash` against the local `facts` table; builds `factIdx → local fact UUID` (reusing existing UUIDs on dedup hits).
5. Inserts `fact_sources`, `concepts`, `concept_aliases`, `fact_concepts`, remapping idxs to local UUIDs.
6. Inserts `concept_summaries`, rewriting `[text](<fact:UUID>)` citations to local UUIDs.
7. Inserts `concept_syntheses`, `investigations`, `investigation_sources`, `reports`, `report_annotations`, remapping idxs.
8. Upserts `embeddings` to Qdrant when the model matches; otherwise enqueues local embed jobs.

No fetch, no chat-model call, no summarization call, no synthesis call. A full curated knowledge graph lands in a fresh repository for the cost of a single streaming JSON parse. This is the format that makes a researched repository a shareable, addressable artifact — and the foundation for cross-instance reuse that the [Decomposition Package](/docs/reference/schemas/decomposition-package) provides at the per-source level.