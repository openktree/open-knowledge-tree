package rbac

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// UserManager is the high-level entry point for user
// operations. It composes the UserStore (the SQL side)
// and the RBAC enforcer (the casbin side) so every
// user mutation that affects roles writes both the
// relational row and the casbin grouping policy in one
// call. The callers (HTTP handlers) never see casbin
// directly.
//
// The dual-write pattern mirrors GroupManager: the
// `users` table is the source of truth for the user
// identity (email, display_name, password_hash), and
// the casbin_rule table is the source of truth for the
// RBAC chain (user → role → domain). Both must be
// updated together; if they drift, the enforce path
// either lets a deleted user keep a role or fails to
// grant a role to a new user. The UserManager keeps
// them in sync by writing both inside the same
// SavePolicy cycle.
type UserManager struct {
	pool  *pgxpool.Pool
	store UserStore
	enf   *Service
}

// NewUserManager builds a manager. The wiring layer
// is responsible for passing the system pool and the
// shared *Service. UserManager does not own either
// (no Close, no Start); it borrows.
func NewUserManager(pool *pgxpool.Pool, svc *Service) *UserManager {
	return &UserManager{
		pool:  pool,
		store: NewPgxUserStore(),
		enf:   svc,
	}
}

// CreateUser inserts a new user row. New users receive no
// default role — they get repo-scoped roles when a sysadmin
// or repoadmin adds them to a repository.
//
// email must be non-empty and unique. The DB enforces
// uniqueness; we surface the duplicate as a pgx
// PgError with code 23505 so the handler can return
// 409.
func (m *UserManager) CreateUser(ctx context.Context, email, passwordHash, displayName string) (User, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return User{}, errors.New("email is required")
	}
	q := store.New(m.pool)
	user, err := m.store.CreateUser(ctx, q, email, passwordHash, displayName)
	if err != nil {
		return User{}, err
	}
	return user, nil
}

// GetUser fetches a user by ID.
func (m *UserManager) GetUser(ctx context.Context, id UserID) (User, error) {
	q := store.New(m.pool)
	u, err := m.store.GetUserByID(ctx, q, id)
	if err != nil {
		if isPgNoRows(err) {
			return User{}, ErrUserNotFound
		}
		return User{}, err
	}
	return u, nil
}

// GetUserByEmail fetches a user by email. Used by the
// login handler to look up the credential row.
func (m *UserManager) GetUserByEmail(ctx context.Context, email string) (User, error) {
	q := store.New(m.pool)
	u, err := m.store.GetUserByEmail(ctx, q, email)
	if err != nil {
		if isPgNoRows(err) {
			return User{}, ErrUserNotFound
		}
		return User{}, err
	}
	return u, nil
}

// ListUsers returns every user, newest first.
func (m *UserManager) ListUsers(ctx context.Context) ([]User, error) {
	q := store.New(m.pool)
	return m.store.ListUsers(ctx, q)
}

// UpdateUser updates the display name. No casbin
// side-effect.
func (m *UserManager) UpdateUser(ctx context.Context, id UserID, displayName string) (User, error) {
	q := store.New(m.pool)
	u, err := m.store.UpdateUser(ctx, q, id, displayName)
	if err != nil {
		if isPgNoRows(err) {
			return User{}, ErrUserNotFound
		}
		return User{}, err
	}
	return u, nil
}

// DeleteUser removes a user row. The casbin side is
// cleaned up by iterating the user's roles and
// removing each grouping policy. The order matters:
// we compute the roles to clean BEFORE the SQL delete
// runs, otherwise the casbin rows are orphaned.
func (m *UserManager) DeleteUser(ctx context.Context, id UserID) error {
	q := store.New(m.pool)

	roles, err := m.enf.GetRolesForUser(string(id), DomainAll)
	if err != nil {
		return fmt.Errorf("listing user roles before delete: %w", err)
	}

	for _, role := range roles {
		if err := m.enf.RemoveGroupingPolicy(string(id), role, DomainAll); err != nil {
			return fmt.Errorf("removing user→role casbin link: %w", err)
		}
	}

	if err := m.store.DeleteUser(ctx, q, id); err != nil {
		return fmt.Errorf("deleting user row: %w", err)
	}
	return nil
}

// AssignRole records a (user, role, domain) tuple and
// writes the casbin grouping policy. The domain is the
// casbin domain (`system` for system-scope roles, a
// repo UUID for repo-scope). The valid roles are the
// same set the admin AssignRole endpoint accepts.
//
// Idempotency: if the (user, role, domain) tuple is
// already granted, the operation is a no-op (the
// underlying AddGroupingPolicy returns false but no
// error). This keeps retry / replay safe.
func (m *UserManager) AssignRole(ctx context.Context, userID string, role, domain string) error {
	if !IsValidRole(role) {
		return fmt.Errorf("invalid role: %s", role)
	}
	if err := m.enf.AddGroupingPolicy(userID, role, domain); err != nil {
		return fmt.Errorf("writing user→role casbin link: %w", err)
	}
	return nil
}

// RemoveRole removes a (user, role, domain) casbin
// grouping policy.
func (m *UserManager) RemoveRole(ctx context.Context, userID string, role, domain string) error {
	if err := m.enf.RemoveGroupingPolicy(userID, role, domain); err != nil {
		return err
	}
	return nil
}
