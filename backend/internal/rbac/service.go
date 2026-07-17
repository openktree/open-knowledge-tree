package rbac

import (
	_ "embed"
	"fmt"
	"sync"

	"github.com/casbin/casbin/v2"
	casbinmodel "github.com/casbin/casbin/v2/model"
)

//go:embed model.conf
var modelConf string

type Service struct {
	enforcer *casbin.Enforcer
	adapter  *PgxAdapter
	mu       sync.RWMutex
}

func NewService(adapter *PgxAdapter) (*Service, error) {
	m, err := casbinmodel.NewModelFromString(modelConf)
	if err != nil {
		return nil, fmt.Errorf("loading casbin model: %w", err)
	}

	e, err := casbin.NewEnforcer(m, adapter)
	if err != nil {
		return nil, fmt.Errorf("creating enforcer: %w", err)
	}

	return &Service{
		enforcer: e,
		adapter:  adapter,
	}, nil
}

func (s *Service) Enforce(userID, repositoryID, resource, action string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if ok, err := s.EnforceSystemAdmin(userID); err != nil {
		return false, err
	} else if ok {
		return true, nil
	}

	// Direct casbin Enforce. The matcher evaluates
	// `g(r.sub, p.sub, r.dom)`, and the default role
	// manager walks the chain: user → ... → role.
	// Group membership adds `g, userID, groupID, *`
	// and `g, groupID, role, *` rows to casbin_rule;
	// the walk transparently resolves user → group →
	// role, so a single Enforce covers both direct
	// role grants and group-inherited roles.
	//
	// We previously did the walk by hand (fetch
	// roles, then Enforce per role). The manual walk
	// was correct for direct grants but blind to
	// groups; the casbin-native walk is correct for
	// both.
	return s.enforcer.Enforce(userID, repositoryID, resource, action)
}

func (s *Service) EnforceSystemAdmin(userID string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	roles := s.enforcer.GetRolesForUserInDomain(userID, DomainSystem)
	for _, role := range roles {
		if role == RoleSysAdmin {
			return true, nil
		}
	}
	return false, nil
}

// CanPickRepositoryDatabase returns true if the user is a system admin.
// Database selection is a system-level admin feature.
func (s *Service) CanPickRepositoryDatabase(userID string) bool {
	ok, _ := s.EnforceSystemAdmin(userID)
	return ok
}

func (s *Service) AddRoleForUser(userID, role, repositoryID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.enforcer.AddGroupingPolicy(userID, role, repositoryID)
	if err != nil {
		return fmt.Errorf("add role policy: %w", err)
	}
	return s.enforcer.SavePolicy()
}

func (s *Service) RemoveRoleForUser(userID, role, repositoryID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.enforcer.RemoveGroupingPolicy(userID, role, repositoryID)
	if err != nil {
		return fmt.Errorf("remove role policy: %w", err)
	}
	return s.enforcer.SavePolicy()
}

func (s *Service) GetRolesForUser(userID string, repositoryID string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	roles := s.enforcer.GetRolesForUserInDomain(userID, repositoryID)
	return roles, nil
}

func (s *Service) GetPermissionsForUser(userID string, repositoryID string) ([][]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	perms, err := s.enforcer.GetImplicitPermissionsForUser(userID, repositoryID)
	if err != nil {
		return nil, fmt.Errorf("get permissions: %w", err)
	}
	return perms, nil
}

func (s *Service) GetAllPermissions() ([][]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.enforcer.GetFilteredPolicy(0)
}

func (s *Service) GetRepositoriesForUser(userID string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	roles, err := s.enforcer.GetRolesForUser(userID)
	if err != nil {
		return nil, fmt.Errorf("get roles: %w", err)
	}

	repoSet := make(map[string]bool)
	for _, role := range roles {
		if role == RoleSysAdmin {
			continue
		}
		domains, _ := s.enforcer.GetModel()["g"]["g"].RM.GetRoles(role)
		for _, domain := range domains {
			repoSet[domain] = true
		}
	}

	var repos []string
	for repo := range repoSet {
		repos = append(repos, repo)
	}
	return repos, nil
}

// AddGroupingPolicy adds a `g, ...` casbin grouping
// policy through the service. Used by the group
// manager to wire (user → group) and (group → role)
// chains into casbin in lock-step with the relational
// group store.
func (s *Service) AddGroupingPolicy(userOrGroup, role, domain string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.enforcer.AddGroupingPolicy(userOrGroup, role, domain)
	if err != nil {
		return fmt.Errorf("add grouping policy: %w", err)
	}
	return s.enforcer.SavePolicy()
}

// RemoveGroupingPolicy removes a `g, ...` casbin
// grouping policy. Returns nil on a no-op (the policy
// did not exist) so callers do not have to
// distinguish "already removed" from "removed now".
func (s *Service) RemoveGroupingPolicy(userOrGroup, role, domain string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.enforcer.RemoveGroupingPolicy(userOrGroup, role, domain)
	if err != nil {
		return fmt.Errorf("remove grouping policy: %w", err)
	}
	return s.enforcer.SavePolicy()
}

// AddPolicy adds a `p, ...` casbin policy.
func (s *Service) AddPolicy(params ...interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.enforcer.AddPolicy(params...)
	if err != nil {
		return fmt.Errorf("add policy: %w", err)
	}
	return s.enforcer.SavePolicy()
}

// RemovePolicy removes a `p, ...` casbin policy.
func (s *Service) RemovePolicy(params ...interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.enforcer.RemovePolicy(params...)
	if err != nil {
		return fmt.Errorf("remove policy: %w", err)
	}
	return s.enforcer.SavePolicy()
}

func (s *Service) LoadPolicy() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enforcer.LoadPolicy()
}

type Permission struct {
	Resource string `json:"resource"`
	Action   string `json:"action"`
}

type UserRole struct {
	Role         string `json:"role"`
	RepositoryID string `json:"repository_id"`
}
