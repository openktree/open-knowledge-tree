/**
 * Constants for the SourceDetail page. The detail page
 * shows the parsed view of a source row plus its image
 * list; nothing in here is concerned with the raw body
 * the worker fetched, which the UI does not display.
 */

// Maps the parse_status string from the database to a
// Badge variant. Unknown values fall through to gray.
export const parseStatusVariant = {
  ok: "green",
  failed: "red",
  unsupported: "yellow",
};

// Human-friendly label for the empty/loading copy on
// the page. The keys match parse_status (with an
// extra "pending" entry for rows that have not been
// parsed yet, distinguished by a NULL parse_status).
export const parseStatusCopy = {
  pending: "Awaiting parse",
  ok: "Parsed",
  failed: "Parse failed",
  unsupported: "No parser for this source type",
};

// Hard cap on the number of inline images we render
// inline before falling back to a "show all" link. The
// list is unbounded in the database; the cap keeps
// pages with hundreds of figures readable.
export const INLINE_IMAGE_PREVIEW_LIMIT = 12;

// Title shown when parsed_title is empty. The page
// uses the URL as a fallback everywhere instead of
// showing "Untitled".
export const UNTITLED_FALLBACK = "Untitled source";

// Maps the oa_status string (from Unpaywall) to a Badge
// variant. The status tells users whether the article is
// open access or paywalled, which explains why the content
// might be incomplete (e.g. "closed" = only the abstract
// was available). Empty/null oa_status (non-DOI sources)
// renders no badge.
export const oaStatusVariant = {
  green: "green",
  gold: "green",
  bronze: "yellow",
  hybrid: "yellow",
  closed: "red",
};

// Human-friendly label for the OA status badge.
export const oaStatusCopy = {
  green: "Open Access (repository)",
  gold: "Open Access (publisher)",
  bronze: "Free to read",
  hybrid: "Open Access (hybrid)",
  closed: "Closed access — partial content",
};
