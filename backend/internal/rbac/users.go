package rbac

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// User is the rbac-package projection of the `users`
// table. It mirrors store.User but uses a typed ID
// (`UserID`) so the rest of the package can refer to
// user identities without scattering pgtype.UUID around.
// PasswordHash is intentionally excluded — the rbac
// projection is the public identity surface, not the
// credential store.
type User struct {
	ID          UserID `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
}

// UserID is a typed alias for user UUID strings. The
// alias is defined here (not in permissions.go) so it
// can be referenced from rbac-package helpers without
// importing a future users package and creating an
// import cycle.
type UserID string

// UserStore is the rbac package's view of the users
// persistence layer. The interface is defined here (not
// in the store package) so the rbac package can be
// wired against any implementation — sqlc, a mock for
// tests, a future read-replica view, etc.
//
// Every method takes a context and a *store.Queries
// for the system pool. The wiring layer is responsible
// for passing the right Queries instance; the rbac
// package never owns a pool.
type UserStore interface {
	CreateUser(ctx context.Context, q *store.Queries, email, passwordHash, displayName string) (User, error)
	GetUserByID(ctx context.Context, q *store.Queries, id UserID) (User, error)
	GetUserByEmail(ctx context.Context, q *store.Queries, email string) (User, error)
	ListUsers(ctx context.Context, q *store.Queries) ([]User, error)
	UpdateUser(ctx context.Context, q *store.Queries, id UserID, displayName string) (User, error)
	DeleteUser(ctx context.Context, q *store.Queries, id UserID) error
	CountUsers(ctx context.Context, q *store.Queries) (int64, error)
}

// PgxUserStore is the production implementation of
// UserStore. It adapts the sqlc-generated store.Queries
// to the interface and does the pgtype ↔ string ID
// translation. The package keeps the conversion in one
// place so the rest of the code never touches pgtype.
type PgxUserStore struct{}

func NewPgxUserStore() *PgxUserStore { return &PgxUserStore{} }

func (s *PgxUserStore) CreateUser(ctx context.Context, q *store.Queries, email, passwordHash, displayName string) (User, error) {
	row, err := q.CreateUser(ctx, store.CreateUserParams{
		Email:        email,
		PasswordHash: passwordHash,
		DisplayName:  displayName,
	})
	if err != nil {
		return User{}, fmt.Errorf("creating user: %w", err)
	}
	return userFromStore(row), nil
}

func (s *PgxUserStore) GetUserByID(ctx context.Context, q *store.Queries, id UserID) (User, error) {
	var uid pgtype.UUID
	if err := uid.Scan(string(id)); err != nil {
		return User{}, fmt.Errorf("invalid user id: %w", err)
	}
	row, err := q.GetUserByID(ctx, uid)
	if err != nil {
		return User{}, err
	}
	return userFromStore(row), nil
}

func (s *PgxUserStore) GetUserByEmail(ctx context.Context, q *store.Queries, email string) (User, error) {
	row, err := q.GetUserByEmail(ctx, email)
	if err != nil {
		return User{}, err
	}
	return userFromStore(row), nil
}

func (s *PgxUserStore) ListUsers(ctx context.Context, q *store.Queries) ([]User, error) {
	rows, err := q.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing users: %w", err)
	}
	out := make([]User, 0, len(rows))
	for _, r := range rows {
		out = append(out, userFromStore(r))
	}
	return out, nil
}

func (s *PgxUserStore) UpdateUser(ctx context.Context, q *store.Queries, id UserID, displayName string) (User, error) {
	var uid pgtype.UUID
	if err := uid.Scan(string(id)); err != nil {
		return User{}, fmt.Errorf("invalid user id: %w", err)
	}
	row, err := q.UpdateUser(ctx, store.UpdateUserParams{ID: uid, DisplayName: displayName})
	if err != nil {
		return User{}, fmt.Errorf("updating user: %w", err)
	}
	return userFromStore(row), nil
}

func (s *PgxUserStore) DeleteUser(ctx context.Context, q *store.Queries, id UserID) error {
	var uid pgtype.UUID
	if err := uid.Scan(string(id)); err != nil {
		return fmt.Errorf("invalid user id: %w", err)
	}
	return q.DeleteUser(ctx, uid)
}

func (s *PgxUserStore) CountUsers(ctx context.Context, q *store.Queries) (int64, error) {
	return q.CountUsers(ctx)
}

// userFromStore converts a sqlc-generated store.User
// row into the rbac-package User projection. Kept
// package-private so the conversion stays in one place.
func userFromStore(r store.User) User {
	return User{
		ID:          UserID(r.ID.String()),
		Email:       r.Email,
		DisplayName: r.DisplayName,
	}
}

// ErrUserNotFound is returned by UserStore lookups
// when the requested user does not exist. Handlers
// should translate this to a 404.
var ErrUserNotFound = errors.New("user not found")
