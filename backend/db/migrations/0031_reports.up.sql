-- 0031_reports.up.sql
--
-- Reports: user-authored rich markdown documents that are
-- automatically annotated with supporting facts from the
-- repository. A report is uploaded as raw text (markdown), an
-- async River job chunks it into sentences, embeds each sentence
-- with the same ai.EmbeddingProvider the facts use, and searches
-- the okt_facts Qdrant collection for similar facts above a
-- configurable threshold. The matches persist in
-- report_annotations so the UI can render each sentence
-- alongside its auto-cited facts (an "autocitation" view).
--
-- Tables live in okt_repository (per-repo data), scoped by
-- repository_id, matching the sources/facts/investigations
-- convention. On a shared (tier-1) database rows for every repo
-- are interleaved and filtered by repository_id; on an
-- isolated/sovereign database only one repo's rows are present.
--
-- This keeps the existing fact/source domain untouched: a
-- report only REFERENCES facts (via fact_id FK), it never
-- mutates them. Deleting a report cascades to its annotations;
-- deleting a fact referenced by a report cascades the annotation
-- row (the sentence simply loses that citation).

CREATE TABLE IF NOT EXISTS okt_repository.reports (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    repository_id UUID NOT NULL REFERENCES okt_system.repositories(id) ON DELETE CASCADE,
    title         TEXT NOT NULL,
    topic         TEXT,
    body_md       TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending','processing','annotated','failed')),
    error         TEXT,
    annotation_job_id TEXT,
    similarity_threshold DOUBLE PRECISION,
    embedded_model TEXT,
    sentence_count INT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_reports_repository_id
    ON okt_repository.reports(repository_id);
CREATE INDEX IF NOT EXISTS idx_reports_status
    ON okt_repository.reports(status);

-- One row per (report sentence -> matched fact), mirroring
-- fact_references for sources. sentence_index keys into the
-- chunker's global sentence array (decomposition.SegmentSentences).
-- sentence_text is stored so the UI can render the annotated body
-- without re-running the chunker client-side (avoids drift if
-- the chunker rules ever change). score is the cosine similarity
-- Qdrant returned (0..1); the UI surfaces it so users can tell
-- how strong each match is. CASCADE on both sides so deleting a
-- report or a referenced fact cleans up automatically.
CREATE TABLE IF NOT EXISTS okt_repository.report_annotations (
    report_id      UUID NOT NULL REFERENCES okt_repository.reports(id) ON DELETE CASCADE,
    sentence_index INT  NOT NULL,
    sentence_text  TEXT NOT NULL,
    fact_id        UUID NOT NULL REFERENCES okt_repository.facts(id) ON DELETE CASCADE,
    score          DOUBLE PRECISION NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (report_id, sentence_index, fact_id)
);
CREATE INDEX IF NOT EXISTS idx_report_annotations_report
    ON okt_repository.report_annotations(report_id, sentence_index);
CREATE INDEX IF NOT EXISTS idx_report_annotations_fact
    ON okt_repository.report_annotations(fact_id);