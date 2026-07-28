---
id: decomposition-package
sidebar_position: 1
title: Decomposition Package
---

# Decomposition Package

The **Decomposition Package** is the per-source interchange format. It is the complete, serializable output of the Knowledge Flow for one source under one chat model: the atomic facts, the concepts and aliases extracted from them, the optional per-concept summary slices, and the optional fact and concept embeddings. Push one of these to the Knowledge Registry and every other OKT instance that later fetches the same URL can pull it down and import it — skipping fetch and the entire decomposition stage (the most expensive part of the pipeline).

**Source of truth:** `registry/internal/model/types.go` — `DecompositionPackage`.

## Design goals

1. **Self-contained.** A consumer needs only the package JSON to reconstruct a source's derived layer. No callback to the producer, no extra fetches.
2. **Dedup-friendly.** Every fact carries a `content_hash` (SHA-256 of normalized fact text). Two instances that decompose the same source with the same model produce the same hashes, so the registry can link facts across sources and an importer can merge an incoming fact into an existing local fact with the same hash instead of duplicating it.
3. **Promptset-tagged.** The package carries a `promptset_hash` at the package level (one hash per decomposition — every fact in a decomposition shares the same promptset, so the hash is a property of the package, not of each fact). A consumer can reject or filter packages whose philosophy isn't in its accepted set, so incompatible decomposition styles never silently mix. The registry can optionally enforce the tag on push via `promptset.enable_validation`.
4. **Model-tagged.** The package records the chat `model_id` that decomposed the source and the `model` + `dimensions` of every embedding block. A consumer always knows whether vectors are reusable as-is or need re-embedding with its local model.
5. **Minimal.** The package carries the derived layer only — no source body, no images. Those live in the separate `SourcePackage` (the registry stores them side-by-side under the same source id). Keeping the decomposition package text-only keeps it small and model-agnostic.

## Top-level shape

```json
{
  "schema_version": 1,
  "model_id": "google/gemma-4-31b-it",
  "decomposed_by": "user@example.com",
  "decomposed_at": "2026-07-28T12:00:00Z",
  "promptset_hash": "ab12...registry-compat-hash",
  "facts": [ /* FactData[] */ ],
  "concepts": [ /* ConceptData[] */ ],
  "summaries": [ /* SummaryData[] — optional */ ],
  "embeddings": { /* EmbeddingData — optional */ },
  "concept_embeddings": { /* EmbeddingData — optional */ }
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `schema_version` | int | yes | Format version. Bumped on backward-incompatible changes; the importer refuses packages newer than it understands. |
| `model_id` | string | yes | The chat model that produced this decomposition, e.g. `google/gemma-4-31b-it`. The registry keys decompositions by `(source_id, model_id)` so one source can carry several. |
| `decomposed_by` | string | no | Identifier of the user/system that ran the decomposition (audit metadata). |
| `decomposed_at` | time (RFC3339) | yes | When the decomposition completed. |
| `promptset_hash` | string | no¹ | The registry-compatibility hash of the philosophy that produced this decomposition (see [the contract below](#the-promptset_hash-contract)). One hash per package — every fact in a decomposition shares the same promptset. ¹ Required when the registry has `promptset.enable_validation: true`; otherwise optional and defaults to empty (legacy accept-all behavior). |
| `facts` | `FactData[]` | yes | The atomic, self-contained facts. May be empty. |
| `concepts` | `ConceptData[]` | yes | The concepts extracted from the facts, with aliases. May be empty. |
| `summaries` | `SummaryData[]` | no | Per-concept summary slices. Omitted when the push level is `facts` (no summarization pushed). |
| `embeddings` | `EmbeddingData` | no | Fact vectors. Omitted when the push level excludes embeddings or the source hasn't been embedded yet. |
| `concept_embeddings` | `EmbeddingData` | no | Concept vectors. Same conditions as `embeddings`, applied to the concept side. |

## `facts[]`

```json
{
  "id": "f3b1...uuid",
  "content": "Mycorrhizal fungi transfer phosphorus from soil to plant roots in exchange for carbon.",
  "content_hash": "9a17...sha256",
  "source_text": "Mycorrhizal fungi transfer phosphorus...",
  "image_id": "",
  "concept_ids": ["c2a1...", "c8f0..."]
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `id` | string (UUID) | yes | The fact's UUID in the producer's repository. The importer reuses it so `fact_hashes` links the same fact across sources and instances. |
| `content` | string | yes | The self-contained atomic fact text. This is the canonical, coreference-resolved form — a reader can understand it without seeing the source. |
| `content_hash` | string (hex) | yes | SHA-256 of the normalized `content`. **The dedup key.** Two facts with the same hash are the same fact regardless of UUID. |
| `source_text` | string | no | The original source span the fact was distilled from. Carries provenance for audit; not used for dedup. |
| `image_id` | string | no | For image facts (`fact_kind: image`), the referenced image's id in the source's `SourcePackage.images`. Empty for text facts. |
| `concept_ids` | string[] | no | UUIDs of the concepts this fact is linked to. Must reference `id`s listed in the same package's `concepts[]`. |

## `concepts[]`

```json
{
  "id": "c2a1...uuid",
  "canonical_name": "Mycorrhiza",
  "context": "organism",
  "ontology_class": "Eukaryote",
  "aliases": ["mycorrhizal fungi", "arbuscular mycorrhiza"]
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `id` | string (UUID) | yes | The concept's UUID in the producer's repository. |
| `canonical_name` | string | yes | Display name. Uniqueness within a repository is `(repository_id, lower(canonical_name), lower(context))`. |
| `context` | string | yes | Ontology label (DBpedia L3 or a custom context). The same surface form under different contexts is a different concept. |
| `ontology_class` | string | no | Higher-level ontology class the concept maps to. |
| `aliases` | string[] | no | Surface forms that resolve to this concept. Used by alias-based concept resolution on import. |

## `summaries[]` (optional)

```json
{
  "id": "s7c0...uuid",
  "concept_id": "c2a1...",
  "slice_number": 1,
  "is_open": false,
  "content": "Mycorrhizal fungi form a mutualistic ... [text](<fact:f3b1...>) ...",
  "fact_ids": ["f3b1...", "f9e2..."]
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `id` | string (UUID) | yes | Summary slice UUID. |
| `concept_id` | string (UUID) | yes | The concept this slice summarizes. Must reference an `id` in `concepts[]`. |
| `slice_number` | int | yes | 1-based index of this slice within the concept's sequence of summaries. |
| `is_open` | bool | yes | `false` = frozen slice, `true` = still accumulating on the producer side. Importers typically treat open slices as informational only. |
| `content` | string | yes | Markdown body. Citations use the `[text](<fact:FACT_ID>)` link form, referencing fact UUIDs from `facts[]`. |
| `fact_ids` | string[] | no | The facts this slice covers. Used to drive incremental re-summarization when facts are added later. |

## `embeddings` / `concept_embeddings` (optional)

```json
{
  "model": "BAAI/bge-large-en-v1.5",
  "dimensions": 1024,
  "vectors": {
    "f3b1...uuid": [0.0123, -0.0456, ...],
    "f9e2...uuid": [0.0789,  0.0231, ...]
  }
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `model` | string | yes | The embedding model that produced the vectors. |
| `dimensions` | int | yes | Vector dimensionality. |
| `vectors` | `map[string][]float32` | yes | Keys are fact UUIDs (for `embeddings`) or concept UUIDs (for `concept_embeddings`); values are the float32 vectors. |

**Reuse vs. re-embed:** On import, if `embeddings.model` matches the consumer's configured embedding model, the vectors are upserted to Qdrant directly — zero embedding cost. If it differs, the importer drops the block and enqueues a local embed job to re-vectorize the facts with its own model. The package is still useful; only the vectors are discarded.

## The `content_hash` contract

The dedup story hinges on `content_hash` being a stable function of the fact's meaning, not its byte form. Producers MUST:

1. Strip trailing/leading whitespace.
2. Collapse internal whitespace runs to a single space.
3. Normalize Unicode to NFC.
4. Compute `SHA-256(normalized_content)` and emit the hex digest.

Consumers MUST treat two facts with the same `content_hash` as the same fact: prefer the locally existing one, link the incoming `fact_sources` row to it, and skip re-embedding. This is what lets the registry's `fact_hashes` table link facts across sources and instances without any string comparison at query time.

## The `promptset_hash` contract

A decomposition is only as good as the philosophy that drove it. Two instances using the same chat model but different promptsets produce structurally similar but semantically divergent facts. The `promptset_hash` field on the **package** (one hash per decomposition — every fact in a decomposition shares the same promptset, so the hash is a property of the package, not of each fact) records which philosophy produced it. This enables two layers of enforcement:

### Registry side: `promptset.enable_validation` (opt-in)

The registry can be configured to **reject** pushes that omit the hash, making it the standardization enforcement point. Off by default for backward compatibility:

```yaml
promptset:
  enable_validation: false   # default; set true to enforce
```

- **`false` (default)** — a push with an empty `promptset_hash` is accepted and stored as NULL. Preserves the legacy behavior for registries that already hold pre-feature decompositions and for contributing backends that haven't configured a promptset.
- **`true`** — a push with an empty `promptset_hash` is rejected with `400 Bad Request` (`promptset_hash required when validation enabled`). Enabling this requires every contributing OKT backend to have a configured promptset resolver (the built-in default promptset counts; a backend with no promptset configured would start receiving 400s on `contribute_source`).

Pre-existing decomposition rows with NULL `promptset_hash` stay pullable even with validation on: validation only gates **new pushes**. The migration that added the column (`registry/db/migrations/0002_promptset_hash.up.sql`) explicitly notes NULL means "predates the promptset feature" and is interpreted as the built-in hash.

### Puller side: the accepted-hash whitelist (always on)

Every OKT instance pulling from the registry filters decompositions by `promptset_hash` via the `RelevanceFilter.AllowsPromptset` rule, which admits a hash if any of these hold:

1. **The hash is empty.** Always accepted (legacy wire format / pre-feature row). This is what keeps an unvalidated registry pullable by everyone.
2. **The hash is in `DefaultAccepted`.** Seeded with `promptset.DefaultRegistryHashes` (the built-in philosophy's hash) so the default promptset is always pullable by every repo, even one that configured a custom active promptset.
3. **The hash is in the repo's `accepted_promptset_hashes`.** The per-repo whitelist an operator maintains when they want to admit additional, non-default philosophies.

A repo with an empty `accepted_promptset_hashes` list accepts **all** hashes (the default-accept semantics) — only when an operator populates the list does it become selective, admitting exactly `DefaultAccepted ∪ accepted_promptset_hashes`. This lets a deployment start permissive and tighten over time, and lets multiple custom philosophies coexist on one registry without cross-contaminating graphs that haven't accepted each other.

### Producing the hash

The hash is the SHA-256 over the **4 registry-shared promptset phases** (fact extraction, image fact extraction, concept extraction, refinement) — see `promptset.RegistryHashPromptset` in `backend/internal/promptset`. Phases that only affect local output (synthesis, summarization, posture, image picker) are deliberately excluded, so two promptsets that differ only in their summarizer collapse to the same registry hash and don't fragment the shared graph. Producers MUST emit the hash of the promptset they actually used; the registry persists it on the `decompositions` row and echoes it on every `DecompRef` so pullers can filter without pulling the full package.

## Versioning

`schema_version` is currently `1`. The version is bumped only on backward-incompatible shape changes. The importer:

- accepts packages with `schema_version <=` its supported version, applying any defined upgrade path;
- rejects packages with `schema_version >` its supported version with a clear error, never silently truncating.

A new optional field does **not** bump the version — `encoding/json` leaves it at the zero value when absent, and consumers treat absent the same as the field's documented default.

## Producing a package

The OKT backend produces a decomposition package in the `contribute_source` River worker after the Knowledge Flow for a source completes (facts stable + deduplicated). The push level is configurable per repository:

- **`facts`** — `facts` + `embeddings` only.
- **`concepts`** (default) — adds `concepts` + `concept_embeddings`.
- Summaries are pushed when the source has them and the level includes them.

A third-party producer needs only to: fetch its source, run its own decomposition into the shape above, compute `content_hash` per the contract, and `POST` it to the registry's `POST /api/v1/sources/{sid}/decompositions/{model}` endpoint (see [Knowledge Registry](/docs/reference/registry#http-api)).

## Consuming a package

The OKT backend consumes a decomposition package in the `retrieve_source` worker. Before fetching a URL, it calls `SearchSource(url, doi)`. On a hit, it pulls the decomposition for an allowed model, deserializes the package, and:

1. Inserts each fact, deduping on `content_hash` against the local `facts` table; reuses the incoming UUID so `fact_hashes` links across sources.
2. Inserts concepts and aliases, resolving collisions on `(canonical_name, context)` by reusing the local concept.
3. Inserts `fact_concepts` rows from each fact's `concept_ids`.
4. Inserts summary slices (when present).
5. Upserts embeddings to Qdrant when the model matches; otherwise enqueues a local embed job.

No fetch, no chat-model call, no embedding-model call (when models match). This is what bends the cost curve: the first instance to research a topic pays the full bill; every later instance pays roughly zero for the overlapping sources.