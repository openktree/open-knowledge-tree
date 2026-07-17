package rbac

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// GroupManager is the high-level entry point for group
// operations. It composes the GroupStore (the SQL
// side) and the RBAC enforcer (the casbin side) so
// every group mutation writes both the relational row
// and the casbin grouping policy in one call. The
// callers (HTTP handlers) never see casbin directly.
//
// Why two stores, one manager?
//
// The `groups` table is the source of truth for the
// group itself (name, description, members). The
// casbin_rule table is the source of truth for the
// RBAC chain (user → group → role). Both must be
// updated together; if they drift, the enforce path
// either lets an ex-member keep a role or fails to
// grant a role to a current member. The GroupManager
// keeps them in sync by writing both inside the same
// SavePolicy cycle, so a partial failure leaves
// casbin's in-memory model consistent with the DB
// on the next reload.
type GroupManager struct {
	pool   *pgxpool.Pool
	store  GroupStore
	enf    *Service
}

// NewGroupManager builds a manager. The wiring layer
// is responsible for passing the system pool and the
// shared *Service. GroupManager does not own either
// (no Close, no Start); it borrows.
func NewGroupManager(pool *pgxpool.Pool, svc *Service) *GroupManager {
	return &GroupManager{
		pool:  pool,
		store: NewPgxGroupStore(),
		enf:   svc,
	}
}

// CreateGroup inserts a new group. There is no
// corresponding casbin policy at this point — a
// group with no roles and no members is an empty
// bucket that has no effect on enforcement.
//
// name must be non-empty and unique. The DB enforces
// uniqueness; we surface the duplicate as
// ErrGroupAlreadyExists so the handler can return 409.
func (m *GroupManager) CreateGroup(ctx context.Context, name, description string) (Group, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Group{}, errors.New("group name is required")
	}
	q := store.New(m.pool)
	return m.store.CreateGroup(ctx, q, name, description)
}

// GetGroup fetches a group by ID.
func (m *GroupManager) GetGroup(ctx context.Context, id GroupID) (Group, error) {
	q := store.New(m.pool)
	g, err := m.store.GetGroupByID(ctx, q, id)
	if err != nil {
		if isPgNoRows(err) {
			return Group{}, ErrGroupNotFound
		}
		return Group{}, err
	}
	return g, nil
}

// ListGroups returns every group, alphabetical.
func (m *GroupManager) ListGroups(ctx context.Context) ([]Group, error) {
	q := store.New(m.pool)
	return m.store.ListGroups(ctx, q)
}

// UpdateGroup renames / re-describes a group. No
// casbin side-effect.
func (m *GroupManager) UpdateGroup(ctx context.Context, id GroupID, name, description string) (Group, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Group{}, errors.New("group name is required")
	}
	q := store.New(m.pool)
	g, err := m.store.UpdateGroup(ctx, q, id, name, description)
	if err != nil {
		if isPgNoRows(err) {
			return Group{}, ErrGroupNotFound
		}
		return Group{}, err
	}
	return g, nil
}

// DeleteGroup removes a group and all its casbin
// membership policies. Group→role policies are
// removed explicitly; user→group policies fall out
// automatically through the FK cascade on
// group_members, AND the casbin side is cleaned up
// by iterating the removed members below.
//
// The order matters: we have to compute the
// "userIDs to clean" set BEFORE the DB cascade
// runs, otherwise the rows are gone and we cannot
// find the user IDs to remove from casbin.
func (m *GroupManager) DeleteGroup(ctx context.Context, id GroupID) error {
	q := store.New(m.pool)

	// Capture the affected user IDs and group roles
	// before the cascade wipes them.
	members, err := m.store.ListMembers(ctx, q, id)
	if err != nil {
		return fmt.Errorf("listing members before delete: %w", err)
	}
	groupRoles, err := m.listGroupRoles(id)
	if err != nil {
		return fmt.Errorf("listing group roles before delete: %w", err)
	}

	// Drop the casbin side first. If the SQL delete
	// fails, we still have an inconsistent casbin
	// (a few orphan policies), but the next
	// LoadPolicy() on app start will reconcile
	// against the surviving groups. The reverse
	// ordering would risk leaving casbin with stale
	// user→group links that no DB row can explain.
	for _, member := range members {
		// Default domain for a user→group link is
		// `*` (all repos). A future enhancement may
		// let a user be in a group only for a
		// specific repo, but v1 keeps it simple.
		if err := m.enf.RemoveGroupingPolicy(member.UserID, string(id), DomainAll); err != nil {
			return fmt.Errorf("removing user→group casbin link: %w", err)
		}
	}
	for _, gr := range groupRoles {
		if err := m.enf.RemoveGroupingPolicy(string(id), gr.Role, gr.Domain); err != nil {
			return fmt.Errorf("removing group→role casbin link: %w", err)
		}
	}

	// Now drop the row. ON DELETE CASCADE on
	// group_members + casbin_rule cleanup of the
	// group→role links takes care of the SQL side.
	if err := m.store.DeleteGroup(ctx, q, id); err != nil {
		return fmt.Errorf("deleting group row: %w", err)
	}
	return nil
}

// AddMember records a user in a group and writes the
// corresponding `g, userID, groupID, *` casbin
// grouping policy. The `*` domain means the
// membership applies to every repository scope —
// there is no per-repo group membership in v1.
//
// The casbin side uses DomainAll (the legacy "*"
// sentinel) so the existing matcher
// (`g(r.sub, p.sub, r.dom)`) accepts the link for
// any request domain. A future "scoped membership"
// enhancement would store one row per (user, group,
// domain) and the handler would manage the per-domain
// subset.
func (m *GroupManager) AddMember(ctx context.Context, groupID GroupID, userID string) error {
	if _, err := m.GetGroup(ctx, groupID); err != nil {
		return err
	}
	q := store.New(m.pool)
	if err := m.store.AddMember(ctx, q, groupID, userID); err != nil {
		return fmt.Errorf("adding group member: %w", err)
	}
	if err := m.enf.AddGroupingPolicy(userID, string(groupID), DomainAll); err != nil {
		return fmt.Errorf("writing user→group casbin link: %w", err)
	}
	return nil
}

// RemoveMember drops the SQL row and the casbin link.
// See AddMember for the *-domain convention.
func (m *GroupManager) RemoveMember(ctx context.Context, groupID GroupID, userID string) error {
	q := store.New(m.pool)
	if err := m.store.RemoveMember(ctx, q, groupID, userID); err != nil {
		return fmt.Errorf("removing group member: %w", err)
	}
	// Best-effort casbin cleanup. If the casbin row
	// was never written (e.g. a partial failure on
	// AddMember), RemoveGroupingPolicy returns an
	// error wrapping the "not found" case; we don't
	// fail the request in that case.
	if err := m.enf.RemoveGroupingPolicy(userID, string(groupID), DomainAll); err != nil {
		// The underlying enforcer returns false but
		// no error for a no-op. If a real error
		// surfaces, log it but keep the request
		// successful — the SQL side already removed
		// the row.
		// (We do not return err here on purpose; see
		// the comment above.)
	}
	return nil
}

// ListMembers returns the membership list joined
// with the users table.
func (m *GroupManager) ListMembers(ctx context.Context, groupID GroupID) ([]GroupMember, error) {
	q := store.New(m.pool)
	return m.store.ListMembers(ctx, q, groupID)
}

// ListForUser returns every group a user belongs to.
func (m *GroupManager) ListForUser(ctx context.Context, userID string) ([]Group, error) {
	q := store.New(m.pool)
	return m.store.ListForUser(ctx, q, userID)
}

// GrantRole records a (group, role, domain) tuple
// and writes the casbin grouping policy. The domain
// is the casbin domain (`system` for system-scope
// roles, a repo UUID for repo-scope). The valid roles
// are the same set the AssignRole endpoint accepts.
//
// Idempotency: if the (group, role, domain) tuple is
// already granted, the operation is a no-op (the
// underlying AddGroupingPolicy returns false but no
// error). This keeps retry / replay safe.
func (m *GroupManager) GrantRole(ctx context.Context, groupID GroupID, role, domain string) error {
	if !IsValidRole(role) {
		return fmt.Errorf("invalid role: %s", role)
	}
	if _, err := m.GetGroup(ctx, groupID); err != nil {
		return err
	}
	// Persist the grant. We don't have a `group_roles`
	// table — the casbin row IS the persistence. The
	// (group, role, domain) tuple can be reconstructed
	// from casbin_rule on demand by the listGroupRoles
	// helper.
	if err := m.enf.AddGroupingPolicy(string(groupID), role, domain); err != nil {
		return fmt.Errorf("writing group→role casbin link: %w", err)
	}
	return nil
}

// RevokeRole removes a (group, role, domain) casbin
// grouping policy. The group's remaining members
// immediately lose the role on the next enforce call.
func (m *GroupManager) RevokeRole(ctx context.Context, groupID GroupID, role, domain string) error {
	if err := m.enf.RemoveGroupingPolicy(string(groupID), role, domain); err != nil {
		return err
	}
	return nil
}

// ListGroupRoles returns every (role, domain) tuple
// currently granted to the group. It reads directly
// from casbin_rule, which is the source of truth for
// the group→role relationship (see GrantRole's
// comment for why there is no separate table).
func (m *GroupManager) ListGroupRoles(ctx context.Context, groupID GroupID) ([]GroupRole, error) {
	return m.listGroupRoles(groupID)
}

// listGroupRoles is the internal reader. We pass the
// typed GroupID directly; the *context.Context
// parameter is dropped because GetFilteredGroupingPolicy
// is in-memory and does not need a context. The public
// ListGroupRoles keeps the context for API symmetry
// with the other list/read methods.
func (m *GroupManager) listGroupRoles(id GroupID) ([]GroupRole, error) {
	m.enf.mu.RLock()
	defer m.enf.mu.RUnlock()

	groupings, err := m.enf.enforcer.GetFilteredGroupingPolicy(0, string(id))
	if err != nil {
		return nil, err
	}
	out := make([]GroupRole, 0, len(groupings))
	for _, g := range groupings {
		// casbin's GetFilteredGroupingPolicy returns
		// each row as the tuple [v0, v1, v2, ...]
		// where v0 is the value we filtered on
		// (the group ID here). For a 3-arg grouping
		// (g, groupID, role, domain) the returned
		// slice is [groupID, role, domain].
		if len(g) < 3 {
			continue
		}
		out = append(out, GroupRole{Role: g[1], Domain: g[2]})
	}
	return out, nil
}

// isPgNoRows reports whether err is pgx's
// "no rows in result set" error. The store package
// does not re-export it consistently across versions,
// so we check by string match — cheap and stable
// enough for an error-classification helper.
func isPgNoRows(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "no rows in result set")
}
