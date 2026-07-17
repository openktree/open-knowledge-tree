/**
 * Maps a source row's `status` field to a Badge variant.
 * The set is small and stable: the schema CHECK constraint
 * (see db/migrations/0004_sources.up.sql) only allows the
 * four values below. Keeping the mapping here means new
 * statuses only need to update this file plus the schema.
 */
export function statusVariant(status) {
  switch (status) {
    case "fetched":
      return "green";
    case "fetching":
      return "blue";
    case "failed":
      return "red";
    case "pending":
    default:
      return "gray";
  }
}

/**
 * Renders a pgtype.Timestamptz-encoded timestamp (the shape
 * sqlc produces for okt_repository.sources.fetched_at and
 * friends) as a short human-readable string. The pgtype
 * shape is { Valid: bool, Time: Date, InfinityModifier: 0,
 * Microseconds: 0 }. The pgtype module ships its own
 * RFC3339 formatter, but we keep this local so the UI
 * doesn't pull another module just for one line.
 */
export function formatTimestamp(ts) {
  if (!ts || !ts.Valid) return "";
  const ms = ts.Time instanceof Date ? ts.Time.getTime() : new Date(ts.Time).getTime();
  if (Number.isNaN(ms)) return "";
  return new Date(ms).toLocaleString();
}

// OA status badge variants and labels. Shared with the
// SourceDetail page. See SourceDetail/constants.js for the
// full description.
export const oaStatusVariant = {
  green: "green",
  gold: "green",
  bronze: "yellow",
  hybrid: "yellow",
  closed: "red",
};

export const oaStatusCopy = {
  green: "Open Access (repository)",
  gold: "Open Access (publisher)",
  bronze: "Free to read",
  hybrid: "Open Access (hybrid)",
  closed: "Closed access — partial content",
};
