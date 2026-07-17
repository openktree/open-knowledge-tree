-- 0020_source_parsed_markdown.up.sql
--
-- Add parsed_markdown column to sources.
--
-- parsed_markdown holds the Markdown rendering of the cleaned
-- article content (trafilatura's ContentNode, converted via
-- html-to-markdown). It is the AI-friendly view of the same
-- content that parsed_text (plain) and parsed_html (raw HTML)
-- represent: it preserves headings, bold/italic emphasis, lists,
-- blockquotes, code blocks and links without the token overhead
-- of raw HTML. The decomposition worker prefers it over
-- parsed_text when feeding the fact extractor, falling back to
-- parsed_text for legacy rows (pre-migration) and PDF sources
-- that have no inline structure to preserve.
--
-- Full-text search stays on parsed_text: Markdown sigils (#, **,
-- -) would pollute tsvector tokenization, so parsed_markdown is
-- AI-only. See 0015_sources_facts_search.up.sql for the search_tsv
-- definition.

ALTER TABLE okt_repository.sources
    ADD COLUMN IF NOT EXISTS parsed_markdown TEXT;