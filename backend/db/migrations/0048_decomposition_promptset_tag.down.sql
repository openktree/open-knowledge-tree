DROP INDEX IF EXISTS okt_repository.idx_fact_references_promptset;
DROP INDEX IF EXISTS okt_repository.idx_fact_concepts_promptset;
DROP INDEX IF EXISTS okt_repository.idx_concepts_promptset;
DROP INDEX IF EXISTS okt_repository.idx_facts_promptset;

ALTER TABLE okt_repository.fact_references
    DROP COLUMN IF EXISTS promptset_hash;

ALTER TABLE okt_repository.fact_concepts
    DROP COLUMN IF EXISTS promptset_hash;

ALTER TABLE okt_repository.concepts
    DROP COLUMN IF EXISTS promptset_hash;

ALTER TABLE okt_repository.facts
    DROP COLUMN IF EXISTS promptset_hash;