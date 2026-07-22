// Action label + badge color map for the audit table. Each action
// is one of the rbac.AuditAction* constants written by the backend.
// The label is human-readable; the badge color groups related
// actions visually (role/group mutations = blue, repo lifecycle =
// green, ingestion = yellow, user = purple, oauth = gray).
export const ACTION_BADGE = {
  role_assign: "blue",
  role_remove: "blue",
  group_create: "blue",
  group_assign: "blue",
  group_remove: "blue",
  grant: "blue",
  revoke: "blue",
  repo_create: "green",
  repo_update: "green",
  repo_delete: "red",
  user_create: "purple",
  user_update: "purple",
  ingestion_start: "yellow",
  oauth_register: "gray",
  oauth_revoke: "gray",
  provider_set: "gray",
};

export const ACTION_LABEL = {
  role_assign: "Role Assign",
  role_remove: "Role Remove",
  group_create: "Group Create",
  group_assign: "Group Assign",
  group_remove: "Group Remove",
  grant: "Grant",
  revoke: "Revoke",
  repo_create: "Repo Create",
  repo_update: "Repo Update",
  repo_delete: "Repo Delete",
  user_create: "User Create",
  user_update: "User Update",
  ingestion_start: "Ingestion Start",
  oauth_register: "OAuth Register",
  oauth_revoke: "OAuth Revoke",
  provider_set: "Provider Set",
};

// formatTime renders an RFC3339 timestamp as a short local string.
// Returns "—" for empty/invalid input.
export function formatTime(ts) {
  if (!ts) return "\u2014";
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  return d.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

// formatDetail renders the JSONB detail column as a compact JSON
// string for the table's expandable row. Returns "{}" for empty.
export function formatDetail(detail) {
  if (!detail || (typeof detail === "object" && Object.keys(detail).length === 0)) {
    return "{}";
  }
  try {
    return JSON.stringify(detail, null, 2);
  } catch {
    return String(detail);
  }
}
