-- graph_import.sql — whole-repository graph import queries.
--
-- The import task (tasks/import_graph.go) reads a GraphBundle and
-- re-inserts every entity into a fresh (mode="new") or existing
-- (mode="existing") repository. The bundle uses internal indices
-- (not UUIDs) for cross-references; the importer remaps each idx to a
-- fresh local UUID (uuid.New) and builds idx→UUID maps as it inserts.
--
-- Low-volume entities (sources, concepts, summaries, syntheses,
-- investigations, reports) reuse the existing idempotent row-by-row
-- queries (CreateSource, CreateConcept, CreateSummary, UpsertSynthesis,
-- CreateInvestigation, CreateReport) because they need the ON CONFLICT
-- DO NOTHING semantics for the existing-repo merge path. The
-- high-volume junctions (facts, fact_sources, fact_concepts,
-- concept_aliases) get batch insert queries here so a 10k-fact graph
-- doesn't cost 10k round-trips.
--
-- All batch inserts use parallel-array unnest so sqlc emits a single
-- *Queries method taking []uuid.UUID / []string / []int32 slices. The
-- caller (the importer) builds the slices in Go and makes one call
-- per entity type. ON CONFLICT DO NOTHING keeps the existing-repo
-- merge path idempotent (a re-import of the same graph is a no-op).

-- name: BatchCreateFacts :execrows
-- Bulk insert facts with fresh UUIDs. The caller passes parallel
-- arrays: ids (fresh uuid.New per fact), texts, fact_kinds,
-- image_urls (nullable — "" = NULL via NULLIF), statuses ('stable'),
-- promptset_hashes. ON CONFLICT DO NOTHING so the existing-repo merge
-- path is a no-op for facts that already exist by id (the new-repo
-- path uses fresh UUIDs so no conflict is possible).
--
-- The arrays are zipped via multiple single-argument unnest calls
-- joined WITH ORDINALITY (sqlc's parser doesn't typeinfer multi-arg
-- unnest, but single-arg unnest with ordinality is the same pattern
-- the existing facts.sql queries use). The arrays MUST be the same
-- length (the caller builds them in lockstep in Go).
INSERT INTO okt_repository.facts (id, text, fact_kind, image_url, status, promptset_hash)
SELECT ids.id, texts.text, fact_kinds.fact_kind,
       NULLIF(image_urls.image_url, ''), statuses.status, promptset_hashes.promptset_hash
FROM unnest($1::uuid[]) WITH ORDINALITY AS ids(id, rn)
JOIN unnest($2::text[]) WITH ORDINALITY AS texts(text, rn) ON texts.rn = ids.rn
JOIN unnest($3::text[]) WITH ORDINALITY AS fact_kinds(fact_kind, rn) ON fact_kinds.rn = ids.rn
JOIN unnest($4::text[]) WITH ORDINALITY AS image_urls(image_url, rn) ON image_urls.rn = ids.rn
JOIN unnest($5::text[]) WITH ORDINALITY AS statuses(status, rn) ON statuses.rn = ids.rn
JOIN unnest($6::text[]) WITH ORDINALITY AS promptset_hashes(promptset_hash, rn) ON promptset_hashes.rn = ids.rn
ON CONFLICT (id) DO NOTHING;

-- name: BatchAddFactSources :execrows
-- Bulk insert fact_sources junction rows. The caller passes parallel
-- arrays: fact_ids, source_ids, chunk_indexes. ON CONFLICT DO NOTHING
-- so a re-import is a no-op (the junction PK is (fact_id, source_id)).
INSERT INTO okt_repository.fact_sources (fact_id, source_id, chunk_index)
SELECT fact_ids.fact_id, source_ids.source_id, chunk_indexes.chunk_index
FROM unnest($1::uuid[]) WITH ORDINALITY AS fact_ids(fact_id, rn)
JOIN unnest($2::uuid[]) WITH ORDINALITY AS source_ids(source_id, rn) ON source_ids.rn = fact_ids.rn
JOIN unnest($3::int4[]) WITH ORDINALITY AS chunk_indexes(chunk_index, rn) ON chunk_indexes.rn = fact_ids.rn
ON CONFLICT (fact_id, source_id) DO NOTHING;

-- name: BatchAddFactConcepts :execrows
-- Bulk insert fact_concepts junction rows. The caller passes parallel
-- arrays: fact_ids, concept_ids, promptset_hashes. ON CONFLICT DO
-- NOTHING so a re-import is a no-op (the junction PK is
-- (fact_id, concept_id)).
INSERT INTO okt_repository.fact_concepts (fact_id, concept_id, promptset_hash)
SELECT fact_ids.fact_id, concept_ids.concept_id, promptset_hashes.promptset_hash
FROM unnest($1::uuid[]) WITH ORDINALITY AS fact_ids(fact_id, rn)
JOIN unnest($2::uuid[]) WITH ORDINALITY AS concept_ids(concept_id, rn) ON concept_ids.rn = fact_ids.rn
JOIN unnest($3::text[]) WITH ORDINALITY AS promptset_hashes(promptset_hash, rn) ON promptset_hashes.rn = fact_ids.rn
ON CONFLICT (fact_id, concept_id) DO NOTHING;

-- name: BatchCreateConceptAliases :execrows
-- Bulk insert concept_aliases. The caller passes parallel arrays:
-- concept_ids, alias_texts. ON CONFLICT DO NOTHING so a re-import is
-- a no-op (the unique index is (concept_id, lower(alias_text))).
INSERT INTO okt_repository.concept_aliases (concept_id, alias_text)
SELECT concept_ids.concept_id, alias_texts.alias_text
FROM unnest($1::uuid[]) WITH ORDINALITY AS concept_ids(concept_id, rn)
JOIN unnest($2::text[]) WITH ORDINALITY AS alias_texts(alias_text, rn) ON alias_texts.rn = concept_ids.rn
ON CONFLICT (concept_id, lower(alias_text)) DO NOTHING;

-- name: BatchAddInvestigationSources :execrows
-- Bulk insert investigation_sources junction rows. The caller passes
-- parallel arrays: investigation_ids, source_ids. ON CONFLICT DO
-- NOTHING so a re-import is a no-op (the junction PK is
-- (investigation_id, source_id)).
INSERT INTO okt_repository.investigation_sources (investigation_id, source_id)
SELECT investigation_ids.investigation_id, source_ids.source_id
FROM unnest($1::uuid[]) WITH ORDINALITY AS investigation_ids(investigation_id, rn)
JOIN unnest($2::uuid[]) WITH ORDINALITY AS source_ids(source_id, rn) ON source_ids.rn = investigation_ids.rn
ON CONFLICT (investigation_id, source_id) DO NOTHING;

-- name: BatchAddReportAnnotations :execrows
-- Bulk insert report_annotations. The caller passes parallel arrays
-- (all NOT NULL except posture). posture is passed as a separate
-- nullable text array; "" maps to NULL via NULLIF. ON CONFLICT DO
-- NOTHING so a re-import is a no-op (the PK is
-- (report_id, sentence_index, fact_id)).
INSERT INTO okt_repository.report_annotations (report_id, sentence_index, sentence_text, fact_id, score, posture)
SELECT report_ids.report_id, sentence_indexes.sentence_index, sentence_texts.sentence_text,
       fact_ids.fact_id, scores.score, NULLIF(postures.posture, '')
FROM unnest($1::uuid[]) WITH ORDINALITY AS report_ids(report_id, rn)
JOIN unnest($2::int4[]) WITH ORDINALITY AS sentence_indexes(sentence_index, rn) ON sentence_indexes.rn = report_ids.rn
JOIN unnest($3::text[]) WITH ORDINALITY AS sentence_texts(sentence_text, rn) ON sentence_texts.rn = report_ids.rn
JOIN unnest($4::uuid[]) WITH ORDINALITY AS fact_ids(fact_id, rn) ON fact_ids.rn = report_ids.rn
JOIN unnest($5::float8[]) WITH ORDINALITY AS scores(score, rn) ON scores.rn = report_ids.rn
JOIN unnest($6::text[]) WITH ORDINALITY AS postures(posture, rn) ON postures.rn = report_ids.rn
ON CONFLICT (report_id, sentence_index, fact_id) DO NOTHING;