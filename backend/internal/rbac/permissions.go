package rbac

import (
	"fmt"
	"strings"
)

// Role identifiers. These are the strings that land in
// casbin_rule.v1 when a user is grouped into a role. Constants
// are defined here so handlers, tests, and seed logic all
// reference the same string set — typo-proof and greppable.
const (
	RoleSysAdmin  = "sysadmin"
	RoleRepoAdmin = "repoadmin"
	RoleEditor    = "editor"
	RoleViewer    = "viewer"
	RoleCurator   = "curator"
)

// Objects are the typed resource strings Casbin uses in
// casbin_rule.v2. Using a struct (rather than a map or a
// loose const block) gives callers compile-time field
// access (`rbac.Objects.Sources`) and IDE completion.
//
// All values are bare resource names. Repository-scoped
// objects take the form `<resource>:<repoID>` and are
// produced by RepoObject() below.
var Objects = struct {
	Users           string
	Roles           string
	Repositories    string
	Members         string
	Sources         string
	Facts           string
	Concepts        string
	SourceProvider  string
	Audit           string
	Groups          string
	AIProvider      string
	Decomposition   string
	AIUsage         string
	Investigations  string
	Tasks           string
	Reports         string
	Databases       string
	Remote          string
	Promptset       string
}{
	Users:           "user",
	Roles:           "role",
	Repositories:    "repository",
	Members:         "member",
	Sources:         "source",
	Facts:           "fact",
	Concepts:        "concept",
	SourceProvider:  "source_provider",
	Audit:           "audit",
	Groups:          "group",
	AIProvider:      "ai_provider",
	Decomposition:   "decomposition",
	AIUsage:         "ai_usage",
	Investigations:  "investigation",
	Tasks:           "task",
	Reports:         "report",
	Databases:       "database",
	Remote:          "remote",
	Promptset:       "promptset",
}

// Actions are the typed action strings Casbin uses in
// casbin_rule.v3.
var Actions = struct {
	Read    string
	Write   string
	Update  string
	Delete  string
	Manage  string
	Execute string
	Cancel  string
}{
	Read:    "read",
	Write:   "write",
	Update:  "update",
	Delete:  "delete",
	Manage:  "manage",
	Execute: "execute",
	Cancel:  "cancel",
}

// Domains (Casbin's "dom" field, stored in casbin_rule.v1 for
// grouping policies and v1 for p policies). The special value
// "*" is the legacy "system / all-repos" sentinel.
const (
	DomainSystem = "system"
	DomainAll    = "*"
)

// Audit actions written to the permission_audit table. The
// action string is the audit row's `action` column; it
// identifies the kind of mutation that happened so audit
// consumers can filter and group without parsing the JSONB
// `policy` payload.
const (
	AuditActionGrant      = "grant"
	AuditActionRevoke     = "revoke"
	AuditActionRoleAssign = "role_assign"
	AuditActionRoleRemove = "role_remove"

	AuditActionGroupCreate = "group_create"
	AuditActionGroupAssign = "group_assign"
	AuditActionGroupRemove = "group_remove"
)

// RepoObject returns the repo-scoped form of a bare resource
// name: e.g. RepoObject("source", "abc-123") -> "source:abc-123".
func RepoObject(resource, repoID string) string {
	if repoID == "" {
		return resource
	}
	return resource + ":" + repoID
}

// ParseRepoObject splits a repo-scoped object string back
// into its parts. Returns (resource, repoID, true) on
// success, ("", "", false) when the string is not in
// the expected `<resource>:<repoID>` form.
func ParseRepoObject(obj string) (resource, repoID string, ok bool) {
	idx := strings.LastIndex(obj, ":")
	if idx == -1 {
		return "", "", false
	}
	return obj[:idx], obj[idx+1:], true
}

// IsValidObject returns true when `obj` is one of the
// known bare resource names.
func IsValidObject(obj string) bool {
	switch obj {
	case Objects.Users,
		Objects.Roles,
		Objects.Repositories,
		Objects.Members,
		Objects.Sources,
		Objects.Facts,
		Objects.Concepts,
		Objects.SourceProvider,
		Objects.Audit,
		Objects.Groups,
		Objects.AIProvider,
		Objects.Decomposition,
		Objects.AIUsage,
		Objects.Investigations,
		Objects.Tasks,
		Objects.Reports,
		Objects.Databases,
		Objects.Remote,
		Objects.Promptset:
		return true
	}
	return false
}

// IsValidAction returns true when `act` is one of the
// known action strings.
func IsValidAction(act string) bool {
	switch act {
	case Actions.Read,
		Actions.Write,
		Actions.Update,
		Actions.Delete,
		Actions.Manage,
		Actions.Execute,
		Actions.Cancel:
		return true
	}
	return false
}

// IsValidRole returns true when `role` is one of the
// recognized role names.
func IsValidRole(role string) bool {
	switch role {
	case RoleSysAdmin,
		RoleRepoAdmin,
		RoleEditor,
		RoleViewer,
		RoleCurator:
		return true
	}
	return false
}

// GroupID is a typed alias for group UUID strings.
type GroupID string

// GroupRoleAssignment describes a single (group, role,
// scope) tuple to be persisted as a Casbin grouping policy.
type GroupRoleAssignment struct {
	GroupID GroupID
	Role    string
	Scope   string
}

func (g GroupRoleAssignment) String() string {
	scope := g.Scope
	if scope == "" {
		scope = DomainSystem
	}
	return fmt.Sprintf("group:%s -> %s @ %s", string(g.GroupID), g.Role, scope)
}
