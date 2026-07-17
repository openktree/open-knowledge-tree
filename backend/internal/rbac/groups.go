package rbac

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// Group is the rbac-package projection of the `groups`
// table. It mirrors store.Group but uses a typed ID
// (`GroupID`) so the rest of the package can refer to
// group identities without scattering pgtype.UUID around.
type Group struct {
	ID          GroupID
	Name        string
	Description string
}

// GroupMember is the rbac projection of a row in
// `group_members` joined with `users`. The email and
// display name are pulled in so the API can return
// the membership list without a second round-trip per
// user.
type GroupMember struct {
	UserID      string
	Email       string
	DisplayName string
	JoinedAt    pgtype.Timestamptz
}

// GroupRole is the projection of a single (group, role,
// domain) tuple persisted as a Casbin grouping policy.
// The rbac service stores these in casbin_rule as
// `(g, groupID, role, domain)`, and Casbin's grouping
// function walks them at enforce time so that every
// member of the group inherits the role.
type GroupRole struct {
	Role   string
	Domain string
}

// GroupStore is the rbac package's view of the groups
// persistence layer. The interface is defined here (not
// in the store package) so the rbac package can be
// wired against any implementation — sqlc, a mock for
// tests, a future read-replica view, etc.
//
// Every method takes a context and a *store.Queries
// for the system pool. The wiring layer is responsible
// for passing the right Queries instance; the rbac
// package never owns a pool.
type GroupStore interface {
	CreateGroup(ctx context.Context, q *store.Queries, name, description string) (Group, error)
	GetGroupByID(ctx context.Context, q *store.Queries, id GroupID) (Group, error)
	GetGroupByName(ctx context.Context, q *store.Queries, name string) (Group, error)
	ListGroups(ctx context.Context, q *store.Queries) ([]Group, error)
	UpdateGroup(ctx context.Context, q *store.Queries, id GroupID, name, description string) (Group, error)
	DeleteGroup(ctx context.Context, q *store.Queries, id GroupID) error
	AddMember(ctx context.Context, q *store.Queries, groupID GroupID, userID string) error
	RemoveMember(ctx context.Context, q *store.Queries, groupID GroupID, userID string) error
	ListMembers(ctx context.Context, q *store.Queries, groupID GroupID) ([]GroupMember, error)
	ListForUser(ctx context.Context, q *store.Queries, userID string) ([]Group, error)
	IsMember(ctx context.Context, q *store.Queries, groupID GroupID, userID string) (bool, error)
}

// PgxGroupStore is the production implementation of
// GroupStore. It adapts the sqlc-generated store.Queries
// to the interface and does the pgtype ↔ string ID
// translation. The package keeps the conversion in one
// place so the rest of the code never touches pgtype.
type PgxGroupStore struct{}

func NewPgxGroupStore() *PgxGroupStore { return &PgxGroupStore{} }

func (s *PgxGroupStore) CreateGroup(ctx context.Context, q *store.Queries, name, description string) (Group, error) {
	row, err := q.CreateGroup(ctx, store.CreateGroupParams{Name: name, Description: description})
	if err != nil {
		return Group{}, fmt.Errorf("creating group: %w", err)
	}
	return groupFromStore(row), nil
}

func (s *PgxGroupStore) GetGroupByID(ctx context.Context, q *store.Queries, id GroupID) (Group, error) {
	var uid pgtype.UUID
	if err := uid.Scan(string(id)); err != nil {
		return Group{}, fmt.Errorf("invalid group id: %w", err)
	}
	row, err := q.GetGroupByID(ctx, uid)
	if err != nil {
		return Group{}, err
	}
	return groupFromStore(row), nil
}

func (s *PgxGroupStore) GetGroupByName(ctx context.Context, q *store.Queries, name string) (Group, error) {
	row, err := q.GetGroupByName(ctx, name)
	if err != nil {
		return Group{}, err
	}
	return groupFromStore(row), nil
}

func (s *PgxGroupStore) ListGroups(ctx context.Context, q *store.Queries) ([]Group, error) {
	rows, err := q.ListGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing groups: %w", err)
	}
	out := make([]Group, 0, len(rows))
	for _, r := range rows {
		out = append(out, groupFromStore(r))
	}
	return out, nil
}

func (s *PgxGroupStore) UpdateGroup(ctx context.Context, q *store.Queries, id GroupID, name, description string) (Group, error) {
	var uid pgtype.UUID
	if err := uid.Scan(string(id)); err != nil {
		return Group{}, fmt.Errorf("invalid group id: %w", err)
	}
	row, err := q.UpdateGroup(ctx, store.UpdateGroupParams{ID: uid, Name: name, Description: description})
	if err != nil {
		return Group{}, fmt.Errorf("updating group: %w", err)
	}
	return groupFromStore(row), nil
}

func (s *PgxGroupStore) DeleteGroup(ctx context.Context, q *store.Queries, id GroupID) error {
	var uid pgtype.UUID
	if err := uid.Scan(string(id)); err != nil {
		return fmt.Errorf("invalid group id: %w", err)
	}
	return q.DeleteGroup(ctx, uid)
}

func (s *PgxGroupStore) AddMember(ctx context.Context, q *store.Queries, groupID GroupID, userID string) error {
	var gid pgtype.UUID
	if err := gid.Scan(string(groupID)); err != nil {
		return fmt.Errorf("invalid group id: %w", err)
	}
	var uid pgtype.UUID
	if err := uid.Scan(userID); err != nil {
		return fmt.Errorf("invalid user id: %w", err)
	}
	return q.AddGroupMember(ctx, store.AddGroupMemberParams{GroupID: gid, UserID: uid})
}

func (s *PgxGroupStore) RemoveMember(ctx context.Context, q *store.Queries, groupID GroupID, userID string) error {
	var gid pgtype.UUID
	if err := gid.Scan(string(groupID)); err != nil {
		return fmt.Errorf("invalid group id: %w", err)
	}
	var uid pgtype.UUID
	if err := uid.Scan(userID); err != nil {
		return fmt.Errorf("invalid user id: %w", err)
	}
	return q.RemoveGroupMember(ctx, store.RemoveGroupMemberParams{GroupID: gid, UserID: uid})
}

func (s *PgxGroupStore) ListMembers(ctx context.Context, q *store.Queries, groupID GroupID) ([]GroupMember, error) {
	var gid pgtype.UUID
	if err := gid.Scan(string(groupID)); err != nil {
		return nil, fmt.Errorf("invalid group id: %w", err)
	}
	rows, err := q.ListGroupMembers(ctx, gid)
	if err != nil {
		return nil, fmt.Errorf("listing group members: %w", err)
	}
	out := make([]GroupMember, 0, len(rows))
	for _, r := range rows {
		out = append(out, GroupMember{
			UserID:      r.UserID.String(),
			Email:       r.Email,
			DisplayName: r.DisplayName,
			JoinedAt:    r.JoinedAt,
		})
	}
	return out, nil
}

func (s *PgxGroupStore) ListForUser(ctx context.Context, q *store.Queries, userID string) ([]Group, error) {
	var uid pgtype.UUID
	if err := uid.Scan(userID); err != nil {
		return nil, fmt.Errorf("invalid user id: %w", err)
	}
	rows, err := q.ListGroupsForUser(ctx, uid)
	if err != nil {
		return nil, fmt.Errorf("listing user groups: %w", err)
	}
	out := make([]Group, 0, len(rows))
	for _, r := range rows {
		out = append(out, groupFromStore(r))
	}
	return out, nil
}

func (s *PgxGroupStore) IsMember(ctx context.Context, q *store.Queries, groupID GroupID, userID string) (bool, error) {
	var gid pgtype.UUID
	if err := gid.Scan(string(groupID)); err != nil {
		return false, fmt.Errorf("invalid group id: %w", err)
	}
	var uid pgtype.UUID
	if err := uid.Scan(userID); err != nil {
		return false, fmt.Errorf("invalid user id: %w", err)
	}
	return q.IsGroupMember(ctx, store.IsGroupMemberParams{GroupID: gid, UserID: uid})
}

// groupFromStore converts a sqlc-generated store.Group
// row into the rbac-package Group projection. Kept
// package-private so the conversion stays in one place.
func groupFromStore(r store.Group) Group {
	return Group{
		ID:          GroupID(r.ID.String()),
		Name:        r.Name,
		Description: r.Description,
	}
}

// ErrGroupNotFound is returned by GroupStore lookups
// when the requested group does not exist. Handlers
// should translate this to a 404.
var ErrGroupNotFound = errors.New("group not found")
