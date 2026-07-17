-- The backfill only inserts; the down simply removes what it added
-- for backfilled repos. Rows for repos created after the feature
-- (seeded by CreateRepository) are NOT touched here, so re-applying
-- the up migration is safe (the NOT EXISTS guards make it idempotent
-- anyway).
DELETE FROM okt_system.repository_provider_settings
 WHERE provider_id IN ('serper','openalex','fetch','unpaywall','tls','flaresolverr')
   AND repository_id IN (SELECT id FROM okt_system.repositories);
-- Note: this down cannot distinguish backfilled context rows from
-- admin-added ones, so it intentionally leaves repository_contexts
-- intact. A full rollback of the feature requires dropping the
-- tables (see 0033 down).