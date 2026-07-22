// Permission options offered in the create-token modal. Kept in
// sync with backend/internal/rbac/permissions.go (rbac.Objects +
// rbac.Actions). The "*" wildcard on either side is allowed by the
// backend validator; the modal exposes the common (object, action)
// pairs plus an "all" shortcut that maps to "*:*".
//
// PERMISSION_CATEGORIES groups the options by resource category
// (mirroring GitHub fine-grained PATs: "Repository permissions",
// "Account permissions", etc.) so the create-token modal renders
// collapsible sections instead of one flat 14-row list. Each
// category has a label + an array of permission options. The
// special "all" entry lives in its own top-level category so it
// stays visually distinct.
export const PERMISSION_CATEGORIES = [
  {
    label: "All permissions",
    description: "Full admin — equivalent to your full RBAC. Use sparingly.",
    options: [{ value: "*:*", label: "All permissions (admin)" }],
  },
  {
    label: "Sources",
    description: "Read, create, or delete sources in the scoped repository.",
    options: [
      { value: "source:read", label: "Read" },
      { value: "source:write", label: "Write" },
      { value: "source:delete", label: "Delete" },
    ],
  },
  {
    label: "Facts",
    description: "Read or write facts extracted from sources.",
    options: [
      { value: "fact:read", label: "Read" },
      { value: "fact:write", label: "Write" },
    ],
  },
  {
    label: "Concepts",
    description: "Read or write the concept graph.",
    options: [
      { value: "concept:read", label: "Read" },
      { value: "concept:write", label: "Write" },
    ],
  },
  {
    label: "Investigations",
    description: "Read or write investigations.",
    options: [
      { value: "investigation:read", label: "Read" },
      { value: "investigation:write", label: "Write" },
    ],
  },
  {
    label: "Reports",
    description: "Read or write reports.",
    options: [
      { value: "report:read", label: "Read" },
      { value: "report:write", label: "Write" },
    ],
  },
  {
    label: "Repositories",
    description: "List or create repositories (system scope).",
    options: [
      { value: "repository:read", label: "Read" },
      { value: "repository:write", label: "Write" },
    ],
  },
];

// PERMISSION_OPTIONS is the flat list kept for back-compat with any
// caller that wants the un-grouped shape (and for tests).
export const PERMISSION_OPTIONS = PERMISSION_CATEGORIES.flatMap((c) => c.options);

// Expiry options offered in the create-token modal (days). 0 = no
// expiry (NULL in the DB). The backend clamps to cfg.api_keys.max_ttl
// (default 90d), so a 365d request silently becomes 90d — that's fine,
// the UI just offers the common shortcuts.
export const EXPIRY_OPTIONS = [
  { value: 0, label: "No expiry" },
  { value: 7, label: "7 days" },
  { value: 30, label: "30 days" },
  { value: 90, label: "90 days" },
];

// ALL_REPOS is the sentinel for "key works on every repo the user can
// access" (repository_id = NULL). Matches the backend's null-repo
// semantics, not a wildcard string, so the create payload omits
// repository_id rather than sending "*".
export const ALL_REPOS = "__all__";

// formatDate renders a pg-style timestamp string (ISO-ish) as a
// human-friendly date. Returns "—" for empty/invalid input.
export function formatDate(ts) {
  if (!ts) return "\u2014";
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return "\u2014";
  return d.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}
