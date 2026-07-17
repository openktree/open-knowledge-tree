---
id: schema
sidebar_position: 6
title: DB Schema
---

# DB Schema

Key tables in the `okt_repository` and `okt_system` schemas. Migrations live in `backend/db/migrations/`.

## okt_repository.sources

The source row: URL, status, parsed content, sentence offsets, DOI, storage info.

| Column | Type | Description |
|--------|------|-------------|
| `id` | UUID PK | |
| `repository_id` | UUID FK | |
| `url` | TEXT | |
| `kind` | TEXT | `text` / `pdf` / etc |
| `status` | TEXT | `pending` / `fetching` / `fetched` / `failed` / `processed` |
| `content` | TEXT | Preview |
| `parsed_title` | TEXT | |
| `parsed_text` | TEXT | |
| `parsed_markdown` | TEXT | Preferred by decomposition |
| `parsed_html` | TEXT | |
| `parsed_author` | TEXT | |
| `parsed_sitename` | TEXT | |
| `parsed_language` | TEXT | |
| `published_at` | DATE | |
| `doi` | TEXT | |
| `oa_status` | TEXT | |
| `sentence_offsets` | INT[] | Flat `[start0,end0,start1,end1,...]` rune offsets of the global sentence array |
| `storage_key` | TEXT | |
| `content_type` | TEXT | |
| `local_path` | TEXT | |
| `stored_at` | TIMESTAMPTZ | |
| `fetch_attempts` | INT | |
| `fetched_at` | TIMESTAMPTZ | |
| `error` | TEXT | |
| `created_at` | TIMESTAMPTZ | |
| `updated_at` | TIMESTAMPTZ | |
| `processed_at` | TIMESTAMPTZ | |

## okt_repository.source_images

Inline images + PDF page renders.

| Column | Type | Description |
|--------|------|-------------|
| `id` | UUID PK | |
| `source_id` | UUID FK CASCADE | |
| `kind` | TEXT | `inline` / `page` |
| `page_number` | INT | (for PDF pages) |
| `position` | INT | |
| `url` | TEXT | |
| `width` / `height` / `bytes` | INT | |
| `local_path` | TEXT | |
| `storage_key` | TEXT | |
| `content_type` | TEXT | |
| `mirrored_at` | TIMESTAMPTZ | |
| `alt_text` | TEXT | |

## okt_repository.facts

Atomic, self-contained facts.

| Column | Type | Description |
|--------|------|-------------|
| `id` | UUID PK | |
| `text` | TEXT | The self-contained fact text |
| `status` | TEXT | `new` / `stable` / `to_delete` |
| `fact_kind` | TEXT | `text` / `image` (default `text`) |
| `image_url` | TEXT | (for image facts) |
| `embedded_at` | TIMESTAMPTZ | When the vector was upserted to Qdrant |
| `embedded_model` | TEXT | |
| `created_at` | TIMESTAMPTZ | |

No `source_id` column — all source links live in the junction.

## okt_repository.fact_sources

Junction: fact &lt;-&gt; source.

| Column | Type |
|--------|------|
| `fact_id` | UUID FK CASCADE |
| `source_id` | UUID FK CASCADE |
| `chunk_index` | INT |
| `first_seen_at` | TIMESTAMPTZ |

PK: `(fact_id, source_id)` (ON CONFLICT idempotent).

## okt_repository.fact_references

Sentence-level provenance.

| Column | Type |
|--------|------|
| `fact_id` | UUID FK |
| `source_id` | UUID FK |
| `sentence_index` | INT |
| `chunk_index` | INT |
| `first_seen_at` | TIMESTAMPTZ |

PK: `(fact_id, source_id, sentence_index)`. Keys into `sources.sentence_offsets`.

## okt_repository.concepts

Concept nodes.

| Column | Type | Description |
|--------|------|-------------|
| `id` | UUID PK | |
| `repository_id` | UUID FK | |
| `canonical_name` | TEXT | Display name |
| `context` | TEXT | Ontology label (DBpedia L3 or custom) |
| `description` | TEXT | |
| `summarizing_at` | TIMESTAMPTZ | Per-concept summarization lock |
| `embedded_at` | TIMESTAMPTZ | |
| `embedded_model` | TEXT | |
| `created_at` | TIMESTAMPTZ | |

Unique: `(repository_id, lower(canonical_name), lower(context))`.

## okt_repository.concept_aliases

Aliases for each concept.

| Column | Type |
|--------|------|
| `id` | UUID PK |
| `concept_id` | UUID FK CASCADE |
| `alias_text` | TEXT |
| `created_at` | TIMESTAMPTZ |

Index on `lower(alias_text)` (lookup); unique `(concept_id, lower(alias_text))`.

## okt_repository.fact_concepts

Junction: fact &lt;-&gt; concept.

| Column | Type |
|--------|------|
| `fact_id` | UUID FK |
| `concept_id` | UUID FK |
| `first_seen_at` | TIMESTAMPTZ |

PK: `(fact_id, concept_id)`.

## okt_repository.concept_summaries

Per-concept summary slices.

| Column | Type | Description |
|--------|------|-------------|
| `id` | UUID PK | |
| `concept_id` | UUID FK | |
| `repository_id` | UUID FK | |
| `context` | TEXT | |
| `sequence_num` | INT | 1-based slice index |
| `is_complete` | BOOLEAN | FALSE=open/accumulating, TRUE=frozen |
| `fact_count` | INT | |
| `content` | TEXT | Markdown with `[text](<fact:fact_id>)` citations |
| `covered_fact_ids` | UUID[] | |
| `model` | TEXT | |
| `created_at` | TIMESTAMPTZ | |
| `updated_at` | TIMESTAMPTZ | |

Unique partial index `uq_concept_summaries_concept_open` on `(concept_id) WHERE is_complete = FALSE`.

## okt_repository.concept_syntheses

One authoritative synthesis per concept group.

| Column | Type | Description |
|--------|------|-------------|
| `id` | UUID PK | |
| `repository_id` | UUID FK | |
| `canonical_name` | TEXT | |
| `content` | TEXT | Markdown synthesis |
| `covered_summary_ids` | UUID[] | |
| `covered_concept_ids` | UUID[] | |
| `embedded_image_ids` | UUID[] | |
| `model` | TEXT | |
| `created_at` | TIMESTAMPTZ | |
| `updated_at` | TIMESTAMPTZ | |

Unique: `(repository_id, lower(canonical_name))`.

## okt_repository.concept_relations

Materialized view — weighted edges between concepts.

| Column | Type |
|--------|------|
| `repository_id` | UUID |
| `name_a` | TEXT (`lower(canonical_name)`) |
| `name_b` | TEXT |
| `shared_fact_count` | INT |

Pairs ordered `name_a < name_b`, self-pairs excluded. Unique for `REFRESH CONCURRENTLY`.

## okt_system.ai_usage

Every LLM call is logged.

| Column | Type |
|--------|------|
| `id` | UUID PK |
| `task_id` | TEXT |
| `model` | TEXT |
| `provider` | TEXT |
| `prompt_tokens` | INT |
| `completion_tokens` | INT |
| `total_tokens` | INT |
| `created_at` | TIMESTAMPTZ |