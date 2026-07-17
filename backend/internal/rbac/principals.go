package rbac

import (
	"context"
	"fmt"

	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// Principal is the unified identity abstraction that
// both User and Group implement. The casbin engine
// already unifies them at enforce time via the g chain
// (g, userID, groupID, * and g, groupID, role, domain);
// this interface unifies them at the management API
// level so callers can list, look up, and display both
// kinds through a single surface.
type Principal interface {
	Kind() PrincipalKind
	PrincipalID() string
	PrincipalDisplayName() string
}

// PrincipalKind discriminates between user and group
// principals. The string value is stable and can be
// used in API responses and audit records.
type PrincipalKind string

const (
	PrincipalKindUser  PrincipalKind = "user"
	PrincipalKindGroup PrincipalKind = "group"
)

// PrincipalStore is the read-only view of principals
// across both kinds. It is defined here so the rbac
// package can be wired against any implementation.
//
// Every method takes a context and a *store.Queries
// for the system pool. The wiring layer is responsible
// for passing the right Queries instance.
type PrincipalStore interface {
	List(ctx context.Context, q *store.Queries, kind PrincipalKind) ([]Principal, error)
	Get(ctx context.Context, q *store.Queries, kind PrincipalKind, id string) (Principal, error)
}

// PgxPrincipalStore is the production implementation of
// PrincipalStore. It delegates to the existing PgxUserStore
// and PgxGroupStore, which in turn adapt the sqlc-generated
// store.Queries. No new DDL or sqlc queries are required.
type PgxPrincipalStore struct {
	users  *PgxUserStore
	groups *PgxGroupStore
}

func NewPgxPrincipalStore() *PgxPrincipalStore {
	return &PgxPrincipalStore{
		users:  NewPgxUserStore(),
		groups: NewPgxGroupStore(),
	}
}

func (s *PgxPrincipalStore) List(ctx context.Context, q *store.Queries, kind PrincipalKind) ([]Principal, error) {
	switch kind {
	case PrincipalKindUser:
		users, err := s.users.ListUsers(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("listing user principals: %w", err)
		}
		out := make([]Principal, 0, len(users))
		for _, u := range users {
			out = append(out, u)
		}
		return out, nil
	case PrincipalKindGroup:
		groups, err := s.groups.ListGroups(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("listing group principals: %w", err)
		}
		out := make([]Principal, 0, len(groups))
		for _, g := range groups {
			out = append(out, g)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown principal kind: %s", kind)
	}
}

func (s *PgxPrincipalStore) Get(ctx context.Context, q *store.Queries, kind PrincipalKind, id string) (Principal, error) {
	switch kind {
	case PrincipalKindUser:
		u, err := s.users.GetUserByID(ctx, q, UserID(id))
		if err != nil {
			return nil, err
		}
		return u, nil
	case PrincipalKindGroup:
		g, err := s.groups.GetGroupByID(ctx, q, GroupID(id))
		if err != nil {
			return nil, err
		}
		return g, nil
	default:
		return nil, fmt.Errorf("unknown principal kind: %s", kind)
	}
}

// UserFromStore converts a sqlc-generated store.User
// row into the rbac-package User projection. This is
// the public counterpart to the package-private
// userFromStore; callers outside the rbac package
// (e.g. tests) use this to convert raw store rows.
func UserFromStore(r store.User) User {
	return userFromStore(r)
}

// GroupFromStore converts a sqlc-generated store.Group
// row into the rbac-package Group projection. This is
// the public counterpart to the package-private
// groupFromStore; callers outside the rbac package
// (e.g. tests) use this to convert raw store rows.
func GroupFromStore(r store.Group) Group {
	return groupFromStore(r)
}

// User implements Principal for the User type.
func (u User) Kind() PrincipalKind              { return PrincipalKindUser }
func (u User) PrincipalID() string              { return string(u.ID) }
func (u User) PrincipalDisplayName() string      { return u.DisplayName }

// Group implements Principal for the Group type.
func (g Group) Kind() PrincipalKind              { return PrincipalKindGroup }
func (g Group) PrincipalID() string              { return string(g.ID) }
func (g Group) PrincipalDisplayName() string     { return g.Name }
