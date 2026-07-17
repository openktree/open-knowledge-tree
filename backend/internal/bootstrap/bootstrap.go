package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/auth"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/rbac"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// SettingsSeeder seeds the per-repository provider + context
// settings for a freshly-created repository. It is the contract
// between EnsureDefaultRepository (which creates the repo row) and
// the handler package (which owns the seeding logic, via
// handler.SeedDefaultRepositorySettings). The bootstrap package
// holds only the signature to avoid an import cycle (bootstrap
// must not import handler). When nil, EnsureDefaultRepository
// skips seeding entirely (the legacy behavior — a default repo
// with no settings, which the per-repo gates deny). Callers that
// want a functional default repo pass a non-nil seeder.
//
// The callback receives the string form of the repository UUID (the
// same value returned in Result.RepositoryID) so the implementation
// can scan it into a pgtype.UUID without the bootstrap package
// depending on pgx.
type SettingsSeeder func(ctx context.Context, repoID string) error

// pgUniqueViolation is the Postgres SQLSTATE for a UNIQUE
// constraint violation. We use it to detect the race where two
// concurrent lazy EnsureDefaultRepository calls both pass the
// "is the table empty?" check and try to insert the same slug.
const pgUniqueViolation = "23505"

// Result holds the outcome of a bootstrap step. It is intentionally
// opaque; callers should check Created or Skipped only.
type Result struct {
	Created      bool
	Skipped      bool
	RepositoryID string
	OwnerID      string
}

// EnsureDefaultRepository creates a single starter repository
// owned by `ownerID` when the repositories table is empty and the
// bootstrap flag is on. The function is safe to call from
// startup (where the caller has no UID and passes the empty
// string) and from the lazy path in GET /repositories (where the
// caller has the authenticated user's UID and wants to attach
// the starter repo to them).
//
// When `ownerID` is empty, the function looks up the
// earliest-created user as a fallback so a manual startup
// invocation still does something useful. The repositories
// table is the source of truth for "does the bootstrap already
// exist?"; if it isn't empty we always skip, regardless of
// ownerID.
//
// The repository is created on the configured default
// repository database (`cfg.Isolation.DefaultDatabase`). The
// `tier` column is set from `Isolation.TierForDatabaseName`; for
// the default database the row is "shared", for any other
// (per-tenant) database it's "isolated".
//
// On a UNIQUE violation (concurrent lazy call won the race) the
// function returns Skipped=true with a nil error so the caller
// refetches the existing row instead of surfacing the conflict
// to the user.
// settings seeds. When nil, the repo is created with no settings
// (the legacy behavior; the per-repo gates will deny everything
// until an admin configures it via the settings UI). Pass a
// non-nil seeder to produce a functional default repo out of the
// box — the production wiring (cmd/app/api.go) and the lazy path
// (internal/api/wiring.go) both pass
// handler.SeedDefaultRepositorySettings so the default repo is
// usable immediately.
func EnsureDefaultRepository(ctx context.Context, registry *dbpool.Registry, cfg *config.Config, ownerID string, seed SettingsSeeder) (Result, error) {
	if !cfg.Bootstrap.DefaultRepository {
		return Result{Skipped: true}, nil
	}

	// The repositories table lives in the system pool. We need it
	// both for the `count` check and for the actual insert.
	systemPool := registry.Get(cfg.System.Database)
	queries := store.New(systemPool.Pool)

	if err := ensureSchema(ctx, systemPool.Pool); err != nil {
		return Result{}, fmt.Errorf("ensuring repository schema: %w", err)
	}

	if ownerID == "" {
		// Startup-time path: pick the earliest user so the
		// repository has an owner out of the box. The
		// lazy path in the repository handler skips this
		// branch by always passing the caller's UID.
		var err error
		ownerID, err = findEarliestUserID(ctx, systemPool.Pool)
		if err != nil {
			return Result{}, fmt.Errorf("finding owner: %w", err)
		}
		if ownerID == "" {
			log.Println("bootstrap: skipping default repository creation — no users in database; the lazy path will create one for the first user that calls GET /repositories")
			return Result{Skipped: true}, nil
		}
	}

	count, err := countRepositories(ctx, systemPool.Pool)
	if err != nil {
		return Result{}, fmt.Errorf("counting repositories: %w", err)
	}
	if count > 0 {
		return Result{Skipped: true}, nil
	}

	repo, err := queries.CreateRepository(ctx, store.CreateRepositoryParams{
		Name:         "Default",
		Slug:         "default",
		Description:  "Auto-created default repository",
		OwnerID:      storeUUID(ownerID),
		DatabaseName: cfg.Isolation.DefaultDatabase,
		Tier:         cfg.Isolation.TierForDatabaseName(cfg.Isolation.DefaultDatabase),
	})
	if err != nil {
		if isUniqueViolation(err) {
			// Another request beat us to it; treat as
			// a no-op so the caller refetches the
			// existing row.
			return Result{Skipped: true}, nil
		}
		return Result{}, fmt.Errorf("creating default repository: %w", err)
	}

	// Seed the per-repository settings (providers + contexts) so
	// the freshly-created default repo is functional out of the
	// box — without this, the search/retrieve gates deny every
	// request (see "search provider not enabled for this
	// repository" in the source handler) and concept extraction
	// hard-fails on the empty context list. The seeder is
	// idempotent (ON CONFLICT DO NOTHING at the sqlc layer), so a
	// retry after a partial failure leaves no half-written rows.
	// A nil seeder preserves the legacy behavior (no settings) so
	// tests that don't care about settings can pass nil.
	if seed != nil {
		if err := seed(ctx, repo.ID.String()); err != nil {
			// Best-effort rollback: delete the repo row so
			// a failed seed doesn't leave an unconfigured
			// repo. The RBAC role grant is orphaned but
			// harmless (the repo is gone).
			_, _ = queries.DeleteRepository(ctx, repo.ID)
			return Result{}, fmt.Errorf("seeding default repository settings: %w", err)
		}
	}

	return Result{
		Created:      true,
		RepositoryID: repo.ID.String(),
		OwnerID:      ownerID,
	}, nil
}

// EnsureDefaultAdmin seeds a system administrator the first time
// the app boots against an empty users table. The credentials come
// from cfg.Bootstrap.DefaultAdminEnv; when the env vars are
// missing the function is a no-op. The function is safe to call
// on every startup: once any user exists, the count check makes
// it a no-op.
//
// The seeded user is granted the system_admin role via the RBAC
// service. We use a service reference instead of writing the
// grouping policy directly so the rest of the application
// (e.g. the admin API) keeps a single source of truth for RBAC
// mutations.
func EnsureDefaultAdmin(ctx context.Context, registry *dbpool.Registry, cfg *config.Config, rbacSvc *rbac.Service) (Result, error) {
	email, password, displayName, ok := cfg.Bootstrap.DefaultAdminEnv()
	if !ok {
		return Result{Skipped: true}, nil
	}

	systemPool := registry.Get(cfg.System.Database)
	if err := ensureSchema(ctx, systemPool.Pool); err != nil {
		return Result{}, fmt.Errorf("ensuring user schema: %w", err)
	}

	queries := store.New(systemPool.Pool)

	count, err := queries.CountUsers(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("counting users: %w", err)
	}
	if count > 0 {
		return Result{Skipped: true}, nil
	}

	hash, err := hashPassword(password)
	if err != nil {
		return Result{}, fmt.Errorf("hashing default admin password: %w", err)
	}

	user, err := queries.CreateUser(ctx, store.CreateUserParams{
		Email:        email,
		PasswordHash: hash,
		DisplayName:  displayName,
	})
	if err != nil {
		// A concurrent first-time boot might have inserted
		// the same email; treat the unique violation as a
		// no-op so we don't crash the API.
		if isUniqueViolation(err) {
			return Result{Skipped: true}, nil
		}
		return Result{}, fmt.Errorf("creating default admin: %w", err)
	}

	if err := rbacSvc.AddRoleForUser(user.ID.String(), rbac.RoleSysAdmin, rbac.DomainSystem); err != nil {
		return Result{}, fmt.Errorf("granting sysadmin role: %w", err)
	}

	log.Printf("bootstrap: seeded default admin %q (%s)", email, rbac.RoleSysAdmin)
	return Result{
		Created: true,
		OwnerID: user.ID.String(),
	}, nil
}

// hashPassword is a thin wrapper around the auth package's
// password hasher so the seeded admin uses the same bcrypt cost
// as self-registered users (and the implementation lives in a
// single place).
func hashPassword(password string) (string, error) {
	return auth.HashPassword(password)
}

// ensureSchema guarantees the okt_system and okt_repository
// schemas exist before the bootstrap reads from them. The
// production startup applies migrations via dbpool.New, but
// tests and the lazy path in the HTTP handler can hit the
// bootstrap before the migration step has run (e.g. when
// `EnsureDefaultRepository` is called from the lazy path in
// GET /repositories on a fresh test database). The CREATE
// statements are no-ops when the schemas already exist.
func ensureSchema(ctx context.Context, pool *pgxpool.Pool) error {
	for _, stmt := range []string{
		`CREATE SCHEMA IF NOT EXISTS okt_system`,
		`CREATE SCHEMA IF NOT EXISTS okt_repository`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func countRepositories(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM repositories`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func findEarliestUserID(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	var id string
	err := pool.QueryRow(ctx, `SELECT id::text FROM users ORDER BY created_at ASC LIMIT 1`).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return id, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == pgUniqueViolation
	}
	return false
}

func storeUUID(s string) pgtype.UUID {
	var uid pgtype.UUID
	_ = uid.Scan(s)
	return uid
}
