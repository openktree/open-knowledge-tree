-- graph_export.sql — whole-repository graph export queries.
--
-- A graph bundle is the serializable form of an entire repository's
-- derived layer: sources + facts + concepts + summaries + syntheses +
-- investigations + reports + embeddings. These queries read every
-- entity for a repository so the export task (tasks/export_graph.go)
-- can assemble a GraphBundle JSON document, gzip it, and push it to
-- the shared knowledge registry. The import task
-- (tasks/import_graph.go) is the inverse: it reads a bundle and
-- re-inserts every entity into a fresh (or existing) repository.
--
-- All queries are repo-scoped via repository_id. The export task
-- resolves the per-repo pool and builds a *store.Queries from it,
-- the same way the per-repo chi middleware does. On a shared (tier-1)
-- database the rows for every repo are interleaved and filtered by
-- repository_id; on an isolated (tier-2/3) database only one repo's
-- rows are physically present.

-- name: ListAllSourcesForExport :many
-- Every source row for a repository, including the parsed
-- text/markdown the import path re-persists so the importing repo
-- doesn't need to re-fetch the URL. Ordered by id for a stable idx
-- assignment in the export builder. (sha256 is computed in Go from
-- the parsed content at bundle-assembly time, not stored on the
-- sources row — the column is registry-side only.)
SELECT id, url, doi, kind, status, parsed_title, parsed_text,
       parsed_markdown, published_at
FROM okt_repository.sources
WHERE repository_id = $1
ORDER BY id;

-- name: ListAllFactsForExport :many
-- Every fact reachable from the repo's sources, with its source
-- links. A fact is repo-scoped via fact_sources → sources (facts has
-- no repository_id column — see 0013_facts.up.sql). DISTINCT because
-- a fact with multiple source links would otherwise appear once per
-- source. content_hash is the dedup key the import path uses to
-- merge facts into an existing repo. promptset_hash tags the fact
-- with the philosophy that produced it. Ordered by id for stable idx.
SELECT DISTINCT ON (f.id) f.id, f.text, f.fact_kind, f.image_url,
       f.status, f.promptset_hash
FROM okt_repository.facts f
JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
JOIN okt_repository.sources s ON fs.source_id = s.id
WHERE s.repository_id = $1
ORDER BY f.id;

-- name: ListAllFactSourcesForExport :many
-- The fact↔source junction, scoped to the repo's facts. Used to
-- rebuild fact_sources on import. chunk_index is per-extraction.
-- Ordered by fact_id, source_id for stable bundle output.
SELECT fs.fact_id, fs.source_id, fs.chunk_index
FROM okt_repository.fact_sources fs
JOIN okt_repository.sources s ON fs.source_id = s.id
WHERE s.repository_id = $1
ORDER BY fs.fact_id, fs.source_id;

-- name: ListAllConceptsForExport :many
-- Every concept row for a repository. promptset_hash tags the
-- concept with the philosophy that produced it. Ordered by id for
-- stable idx assignment.
SELECT id, canonical_name, context, description, promptset_hash
FROM okt_repository.concepts
WHERE repository_id = $1
ORDER BY id;

-- name: ListAllConceptAliasesForExport :many
-- Every alias for the repo's concepts. Ordered by concept_id, alias_text
-- for stable bundle output.
SELECT ca.concept_id, ca.alias_text
FROM okt_repository.concept_aliases ca
JOIN okt_repository.concepts c ON ca.concept_id = c.id
WHERE c.repository_id = $1
ORDER BY ca.concept_id, ca.alias_text;

-- name: ListAllFactConceptsForExport :many
-- The fact↔concept junction, scoped to the repo's concepts. promptset_hash
-- tags the link with the philosophy that produced the fact+concept pair.
-- Ordered by fact_id, concept_id for stable bundle output.
SELECT fc.fact_id, fc.concept_id, fc.promptset_hash
FROM okt_repository.fact_concepts fc
JOIN okt_repository.concepts c ON fc.concept_id = c.id
WHERE c.repository_id = $1
ORDER BY fc.fact_id, fc.concept_id;

-- name: ListAllSummariesForExport :many
-- Every concept_summary slice for the repo's concepts. covered_fact_ids
-- is the array of fact_ids the slice folds in; the import path remaps
-- them to fresh local UUIDs. Ordered by concept_id, sequence_num for
-- stable bundle output.
SELECT cs.id, cs.concept_id, cs.context, cs.sequence_num, cs.is_complete,
       cs.fact_count, cs.content, cs.covered_fact_ids, cs.model
FROM okt_repository.concept_summaries cs
JOIN okt_repository.concepts c ON cs.concept_id = c.id
WHERE c.repository_id = $1
ORDER BY cs.concept_id, cs.sequence_num;

-- name: ListAllSynthesesForExport :many
-- Every concept_synthesis for the repo. canonical_name is the group
-- key (one synthesis per lower(canonical_name) per repo). The import
-- path upserts via UpsertSynthesis (ON CONFLICT on the unique index).
-- Ordered by canonical_name for stable bundle output.
SELECT id, canonical_name, content, covered_summary_ids,
       covered_concept_ids, embedded_image_ids, model
FROM okt_repository.concept_syntheses
WHERE repository_id = $1
ORDER BY canonical_name;

-- name: ListAllInvestigationsForExport :many
-- Every investigation for a repository. Ordered by id for stable idx.
SELECT id, title, topic
FROM okt_repository.investigations
WHERE repository_id = $1
ORDER BY id;

-- name: ListAllInvestigationSourcesForExport :many
-- The investigation↔source junction, scoped to the repo's
-- investigations. Ordered by investigation_id, source_id for stable
-- bundle output.
SELECT is_join.investigation_id, is_join.source_id
FROM okt_repository.investigation_sources is_join
JOIN okt_repository.investigations i ON is_join.investigation_id = i.id
WHERE i.repository_id = $1
ORDER BY is_join.investigation_id, is_join.source_id;

-- name: ListAllReportsForExport :many
-- Every report for a repository, including the parent_id nesting
-- (migration 0043). Ordered by id for stable idx assignment.
SELECT id, title, topic, body_md, status, parent_id,
       similarity_threshold, embedded_model, sentence_count
FROM okt_repository.reports
WHERE repository_id = $1
ORDER BY id;

-- name: ListAllReportAnnotationsForExport :many
-- Every report_annotation for the repo's reports. posture is the
-- autocite classifier label (NULL on legacy/fallback rows). The
-- import path remaps fact_id to the fresh local UUID. Ordered by
-- report_id, sentence_index, fact_id for stable bundle output.
SELECT ra.report_id, ra.sentence_index, ra.sentence_text, ra.fact_id,
       ra.score, ra.posture
FROM okt_repository.report_annotations ra
JOIN okt_repository.reports r ON ra.report_id = r.id
WHERE r.repository_id = $1
ORDER BY ra.report_id, ra.sentence_index, ra.fact_id;

-- name: ListAllFactIDsForExport :many
-- The fact_ids for the repo (for the Qdrant vector fetch). Returns
-- bare ids so the export task can batch them into GetFactVectorsByIDs.
SELECT DISTINCT ON (f.id) f.id
FROM okt_repository.facts f
JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
JOIN okt_repository.sources s ON fs.source_id = s.id
WHERE s.repository_id = $1
ORDER BY f.id;

-- name: ListAllConceptIDsForExport :many
-- The concept_ids for the repo (for the Qdrant concept vector fetch).
SELECT id
FROM okt_repository.concepts
WHERE repository_id = $1
ORDER BY id;