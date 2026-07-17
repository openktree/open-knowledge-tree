// Status + sort options shared by the Facts page index and
// content. Centralized so a future "add a new status" change is
// one edit here instead of a search across files.

export const STATUS_OPTIONS = [
  { value: "stable", label: "Stable" },
  { value: "new", label: "New" },
  { value: "all", label: "All" },
];

// sort=created_at is the default (newest first — the SQL ORDER BY
// is f.created_at DESC). sort=source_count re-sorts the bounded
// result set in-memory by source_count desc so the dedup weight
// signal surfaces as an explicit user toggle.
export const SORT_OPTIONS = [
  { value: "created_at", label: "Newest" },
  { value: "source_count", label: "Most confirmed" },
];