// Badge variant used for each role in the users table.
export const ROLE_BADGE = {
  sysadmin: "red",
  repoadmin: "purple",
  editor: "blue",
  curator: "amber",
  viewer: "green",
};

// The set of roles that can be assigned from the Assign Role form.
// Kept in sync with the backend Casbin policies.
export const ASSIGNABLE_ROLES = [
  { value: "sysadmin", label: "System Admin" },
  { value: "repoadmin", label: "Repo Admin" },
  { value: "editor", label: "Editor" },
  { value: "curator", label: "Curator" },
  { value: "viewer", label: "Viewer" },
];

// Wildcard repository id (matches all repos).
export const ALL_REPOSITORIES = "*";
