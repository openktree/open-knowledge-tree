package rbac

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedPolicies writes the default policy set the first time
// the application boots against an empty casbin_rule table.
//
// Role model:
//
//	sysadmin — explicit system-level policies (no *:* wildcard).
//	  The EnforceSystemAdmin short-circuit handles runtime
//	  enforcement; these policies serve as documentation/audit.
//
//	repository — four object-typed roles. None grant `*:*`.
//	  repoadmin gets full repo control. editor gets sources +
//	  investigations + reports write. curator reads sources and
//	  writes investigations + reports. viewer is read-only.
func seedPolicies(svc *Service) error {
	policies := defaultPolicies()
	groupings := defaultGroupingPolicies()

	for _, p := range policies {
		args := toIfaceSlice(p)
		if _, err := svc.enforcer.AddPolicy(args...); err != nil {
			return err
		}
	}
	if err := svc.enforcer.SavePolicy(); err != nil {
		return err
	}

	for _, g := range groupings {
		args := toIfaceSlice(g)
		if _, err := svc.enforcer.AddGroupingPolicy(args...); err != nil {
			return err
		}
	}
	if err := svc.enforcer.SavePolicy(); err != nil {
		return err
	}

	return nil
}

func toIfaceSlice(in []string) []interface{} {
	out := make([]interface{}, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

func defaultPolicies() [][]string {
	p := make([][]string, 0, 104)

	// ── sysadmin (system scope — explicit, no *:*) ──────────────
	p = append(p,
		[]string{RoleSysAdmin, "*", Objects.Users, Actions.Read},
		[]string{RoleSysAdmin, "*", Objects.Roles, Actions.Read},
		[]string{RoleSysAdmin, "*", Objects.Roles, Actions.Manage},
		[]string{RoleSysAdmin, "*", Objects.Groups, Actions.Read},
		[]string{RoleSysAdmin, "*", Objects.Groups, Actions.Manage},
		[]string{RoleSysAdmin, "*", Objects.Repositories, Actions.Read},
		[]string{RoleSysAdmin, "*", Objects.Repositories, Actions.Write},
		[]string{RoleSysAdmin, "*", Objects.Repositories, Actions.Update},
		[]string{RoleSysAdmin, "*", Objects.Repositories, Actions.Delete},
		[]string{RoleSysAdmin, "*", Objects.Repositories, Actions.Manage},
		[]string{RoleSysAdmin, "*", Objects.Databases, Actions.Read},
		[]string{RoleSysAdmin, "*", Objects.SourceProvider, Actions.Read},
		[]string{RoleSysAdmin, "*", Objects.SourceProvider, Actions.Execute},
		[]string{RoleSysAdmin, "*", Objects.AIProvider, Actions.Read},
		[]string{RoleSysAdmin, "*", Objects.AIProvider, Actions.Execute},
		[]string{RoleSysAdmin, "*", Objects.AIUsage, Actions.Read},
		[]string{RoleSysAdmin, "*", Objects.Decomposition, Actions.Read},
		[]string{RoleSysAdmin, "*", Objects.Tasks, Actions.Read},
		[]string{RoleSysAdmin, "*", Objects.Tasks, Actions.Cancel},
		[]string{RoleSysAdmin, "*", Objects.Tasks, Actions.Manage},
		[]string{RoleSysAdmin, "*", Objects.Audit, Actions.Read},
		[]string{RoleSysAdmin, "*", Objects.Promptset, Actions.Manage},
	)

	// ── repoadmin (repo scope + system-scope utility access) ────
	p = append(p,
		[]string{RoleRepoAdmin, "*", Objects.SourceProvider, Actions.Read},
		[]string{RoleRepoAdmin, "*", Objects.SourceProvider, Actions.Execute},
		[]string{RoleRepoAdmin, "*", Objects.AIProvider, Actions.Read},
		[]string{RoleRepoAdmin, "*", Objects.AIProvider, Actions.Execute},
		[]string{RoleRepoAdmin, "*", Objects.Decomposition, Actions.Read},
		[]string{RoleRepoAdmin, "*", Objects.Repositories, Actions.Read},
		[]string{RoleRepoAdmin, "*", Objects.Repositories, Actions.Update},
		[]string{RoleRepoAdmin, "*", Objects.Repositories, Actions.Delete},
		[]string{RoleRepoAdmin, "*", Objects.Repositories, Actions.Manage},
		[]string{RoleRepoAdmin, "*", Objects.Members, Actions.Read},
		[]string{RoleRepoAdmin, "*", Objects.Members, Actions.Manage},
		[]string{RoleRepoAdmin, "*", Objects.Sources, Actions.Read},
		[]string{RoleRepoAdmin, "*", Objects.Sources, Actions.Write},
		[]string{RoleRepoAdmin, "*", Objects.Sources, Actions.Update},
		[]string{RoleRepoAdmin, "*", Objects.Sources, Actions.Delete},
		[]string{RoleRepoAdmin, "*", Objects.Facts, Actions.Read},
		[]string{RoleRepoAdmin, "*", Objects.Concepts, Actions.Read},
		[]string{RoleRepoAdmin, "*", Objects.Investigations, Actions.Read},
		[]string{RoleRepoAdmin, "*", Objects.Investigations, Actions.Write},
		[]string{RoleRepoAdmin, "*", Objects.Investigations, Actions.Update},
		[]string{RoleRepoAdmin, "*", Objects.Investigations, Actions.Delete},
		[]string{RoleRepoAdmin, "*", Objects.Reports, Actions.Read},
		[]string{RoleRepoAdmin, "*", Objects.Reports, Actions.Write},
		[]string{RoleRepoAdmin, "*", Objects.Reports, Actions.Update},
		[]string{RoleRepoAdmin, "*", Objects.Reports, Actions.Delete},
		[]string{RoleRepoAdmin, "*", Objects.Remote, Actions.Read},
		[]string{RoleRepoAdmin, "*", Objects.Remote, Actions.Write},
		[]string{RoleRepoAdmin, "*", Objects.Tasks, Actions.Read},
		[]string{RoleRepoAdmin, "*", Objects.Tasks, Actions.Cancel},
		[]string{RoleRepoAdmin, "*", Objects.Promptset, Actions.Manage},
	)

	// ── editor (sources + investigations + reports write) ───────
	p = append(p,
		[]string{RoleEditor, "*", Objects.SourceProvider, Actions.Read},
		[]string{RoleEditor, "*", Objects.SourceProvider, Actions.Execute},
		[]string{RoleEditor, "*", Objects.AIProvider, Actions.Read},
		[]string{RoleEditor, "*", Objects.AIProvider, Actions.Execute},
		[]string{RoleEditor, "*", Objects.Decomposition, Actions.Read},
		[]string{RoleEditor, "*", Objects.Repositories, Actions.Read},
		[]string{RoleEditor, "*", Objects.Sources, Actions.Read},
		[]string{RoleEditor, "*", Objects.Sources, Actions.Write},
		[]string{RoleEditor, "*", Objects.Sources, Actions.Update},
		[]string{RoleEditor, "*", Objects.Sources, Actions.Delete},
		[]string{RoleEditor, "*", Objects.Facts, Actions.Read},
		[]string{RoleEditor, "*", Objects.Concepts, Actions.Read},
		[]string{RoleEditor, "*", Objects.Investigations, Actions.Read},
		[]string{RoleEditor, "*", Objects.Investigations, Actions.Write},
		[]string{RoleEditor, "*", Objects.Investigations, Actions.Update},
		[]string{RoleEditor, "*", Objects.Investigations, Actions.Delete},
		[]string{RoleEditor, "*", Objects.Reports, Actions.Read},
		[]string{RoleEditor, "*", Objects.Reports, Actions.Write},
		[]string{RoleEditor, "*", Objects.Reports, Actions.Update},
		[]string{RoleEditor, "*", Objects.Reports, Actions.Delete},
		[]string{RoleEditor, "*", Objects.Remote, Actions.Read},
		[]string{RoleEditor, "*", Objects.Remote, Actions.Write},
		[]string{RoleEditor, "*", Objects.Tasks, Actions.Read},
		[]string{RoleEditor, "*", Objects.Tasks, Actions.Cancel},
		[]string{RoleEditor, "*", Objects.Promptset, Actions.Manage},
	)

	// ── curator (read sources, write investigations + reports) ──
	p = append(p,
		[]string{RoleCurator, "*", Objects.SourceProvider, Actions.Read},
		[]string{RoleCurator, "*", Objects.AIProvider, Actions.Read},
		[]string{RoleCurator, "*", Objects.AIProvider, Actions.Execute},
		[]string{RoleCurator, "*", Objects.Decomposition, Actions.Read},
		[]string{RoleCurator, "*", Objects.Repositories, Actions.Read},
		[]string{RoleCurator, "*", Objects.Sources, Actions.Read},
		[]string{RoleCurator, "*", Objects.Facts, Actions.Read},
		[]string{RoleCurator, "*", Objects.Concepts, Actions.Read},
		[]string{RoleCurator, "*", Objects.Investigations, Actions.Read},
		[]string{RoleCurator, "*", Objects.Investigations, Actions.Write},
		[]string{RoleCurator, "*", Objects.Investigations, Actions.Update},
		[]string{RoleCurator, "*", Objects.Investigations, Actions.Delete},
		[]string{RoleCurator, "*", Objects.Reports, Actions.Read},
		[]string{RoleCurator, "*", Objects.Reports, Actions.Write},
		[]string{RoleCurator, "*", Objects.Reports, Actions.Update},
		[]string{RoleCurator, "*", Objects.Reports, Actions.Delete},
		[]string{RoleCurator, "*", Objects.Remote, Actions.Read},
		[]string{RoleCurator, "*", Objects.Tasks, Actions.Read},
		[]string{RoleCurator, "*", Objects.Tasks, Actions.Cancel},
		[]string{RoleCurator, "*", Objects.Promptset, Actions.Manage},
	)

	// ── viewer (read-only) ─────────────────────────────────────
	p = append(p,
		[]string{RoleViewer, "*", Objects.SourceProvider, Actions.Read},
		[]string{RoleViewer, "*", Objects.AIProvider, Actions.Read},
		[]string{RoleViewer, "*", Objects.Decomposition, Actions.Read},
		[]string{RoleViewer, "*", Objects.Repositories, Actions.Read},
		[]string{RoleViewer, "*", Objects.Sources, Actions.Read},
		[]string{RoleViewer, "*", Objects.Facts, Actions.Read},
		[]string{RoleViewer, "*", Objects.Concepts, Actions.Read},
		[]string{RoleViewer, "*", Objects.Investigations, Actions.Read},
		[]string{RoleViewer, "*", Objects.Reports, Actions.Read},
		[]string{RoleViewer, "*", Objects.Remote, Actions.Read},
		[]string{RoleViewer, "*", Objects.Tasks, Actions.Read},
		[]string{RoleViewer, "*", Objects.Promptset, Actions.Manage},
	)

	return p
}

// defaultGroupingPolicies returns the self-link groupings
// for role resolution. No legacy aliases — clean break.
func defaultGroupingPolicies() [][]string {
	return [][]string{}
}

// SetupRBAC is the boot-time entry point. It opens the
// pgx-backed casbin adapter, loads any persisted policies,
// and seeds the default set if the table is empty.
func SetupRBAC(pool *pgxpool.Pool) (*Service, error) {
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `SELECT count(*) FROM casbin_rule`); err != nil {
		return nil, fmt.Errorf("checking casbin_rule table: %w", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM casbin_rule`).Scan(&count); err != nil {
		return nil, fmt.Errorf("counting casbin rules: %w", err)
	}

	adapter := NewPgxAdapter(pool)
	svc, err := NewService(adapter)
	if err != nil {
		return nil, err
	}

	if count == 0 {
		if err := seedPolicies(svc); err != nil {
			return nil, fmt.Errorf("seeding policies: %w", err)
		}
	}

	return svc, nil
}
