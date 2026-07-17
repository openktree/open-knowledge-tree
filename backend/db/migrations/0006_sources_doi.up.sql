-- 0006_sources_doi.up.sql
--
-- Add a bare DOI column to okt_repository.sources. The column
-- is nullable: most sources (homepages, dataset URLs, generic
-- PDFs) don't have a DOI, only scholarly works do. The
-- classifier in internal/providers/fetch/classify.go extracts
-- the bare DOI from a doi.org URL on the cheap string pass;
-- the OpenAlex search provider also returns the bare DOI on
-- every search hit, and the worker passes it through to the
-- row when the user fetches a search result by DOI.
--
-- Keeping the DOI as a separate column (not just storing it
-- inside the URL) means the UI can render a "DOI: 10.123/foo"
-- pill, deduplicate sources across providers, and link
-- out to doi.org without re-parsing the URL on every read.

ALTER TABLE okt_repository.sources
    ADD COLUMN IF NOT EXISTS doi TEXT;
