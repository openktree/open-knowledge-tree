-- 0045_backfill_silent_failures.up.sql
--
-- One-time backfill: flip fetched/processed rows whose parsed_text
-- matches a known boilerplate / captcha / cookies-disabled /
-- challenge-page signature to status='failed' so the UI flags them
-- instead of presenting the boilerplate as if it were the article
-- body. This is the retroactive companion to the boilerplate guard
-- expansion in internal/providers/fetch/resolution.go: the guard
-- prevents *new* silent failures; this migration surfaces the ~80
-- rows that already slipped through before the guard was widened.
--
-- Why a migration and not a runtime backfill loop:
--   * The set is bounded (the guard now catches every new fetch),
--     so a one-shot UPDATE is the simplest correct tool.
--   * Running it in a migration guarantees it applies to every
--     database (default + any tier-2/3 mirrors) per the
--     multi-database layout rules in AGENTS.md, with the same
--     idempotency guarantee: the WHERE clause matches zero rows
--     after the first run, so re-applying migrations to a fresh
--     mirror is a no-op on the backfill itself.
--
-- What the rows become:
--   * status: 'failed' (was 'fetched' or 'processed').
--   * parse_status: 'failed' (was 'ok'). The UI hides the parsed
--     view on 'failed' rows, so the boilerplate snippet no longer
--     renders as if it were the article.
--   * error: 'silent boilerplate detected on backfill'. Operators
--     see this in the error column and know the row needs a retry,
--     not a manual content edit.
--   * parsed_text, parsed_title, parsed_html, parsed_markdown are
--     left intact so the operator can inspect what the parser
--     produced and confirm the boilerplate signature before
--     retrying. A follow-up cleanup task can blank them if desired.
--   * Facts already extracted from these rows are NOT touched.
--     The "Yes, flip to failed" decision (per the investigation
--     plan) deliberately leaves facts alone: a separate cleanup
--     pass can remove facts whose text matches a boilerplate
--     signature, but that's an invasive change best done
--     explicitly once the operator has reviewed the affected
--     rows. Linking the facts to a 'failed' source is harmless —
--     the decomposition worker re-runs on a successful retry and
--     dedup collapses any regenerated facts onto the survivors.
--
-- The ILIKE patterns mirror the phrases in
-- internal/providers/fetch/resolution.go's jsBoilerplatePhrases
-- plus the IsHTMLLeakBoilerplate prefix check. Keep the two in
-- sync when adding a new phrase: the Go guard prevents new
-- occurrences, this migration's WHERE clause is what surfaces
-- the historical ones.

UPDATE okt_repository.sources
SET status       = 'failed',
    parse_status = 'failed',
    error        = 'silent boilerplate detected on backfill',
    updated_at   = now()
WHERE status IN ('fetched', 'processed')
  AND (
        parsed_text ILIKE '%please help us confirm that you are not a robot%'
     OR parsed_text ILIKE '%could not validate captcha%'
     OR parsed_text ILIKE '%experiencing unusual traffic%'
     OR parsed_text ILIKE '%validate user%'
     OR parsed_text ILIKE '%cookies are disabled%'
     OR parsed_text ILIKE '%requires cookies for authentication%'
     OR parsed_text ILIKE '%making sure you''re not a bot%'
     OR parsed_text ILIKE '%please verify you are a human%'
     OR parsed_text ILIKE '%site protection%verifying your request%'
     OR parsed_text ILIKE '%dear visitor%fight cybercrime%'
     OR parsed_text ILIKE '%connection timed out%error code%'
     OR parsed_text ILIKE '%the page isn''t redirecting properly%'
     OR parsed_text ILIKE '<iframe title="Google Tag Manager"%'
     OR parsed_text ILIKE '<iframe title=''Google Tag Manager''%'
  );