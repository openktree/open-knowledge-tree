-- Reverse of 0061_concept_groups: drop the summary table and its indexes.

DROP INDEX IF EXISTS okt_repository.idx_concept_groups_repo_count_name;
DROP TABLE IF EXISTS okt_repository.concept_groups;