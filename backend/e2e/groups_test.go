//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/rbac"
)

// TestGroupsCreateReadUpdateDelete covers the basic
// group lifecycle as a sysadmin. Verifies that
// creation returns 201, read returns the same
// payload, update applies the new fields, and delete
// returns 200.
//
// bootstrapSysAdmin lives in multi_db_test.go; the
// groups tests reuse it.
func TestGroupsCreateReadUpdateDelete(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "groups-admin@example.com")

	// 1) create
	body, _ := json.Marshal(map[string]string{
		"name":        "lab-alice",
		"description": "Alice's lab",
	})
	resp, raw := admin.do("POST", "/api/v1/groups", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create group: expected 201, got %d: %s", resp.StatusCode, raw)
	}
	var created struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(raw, &created)
	if created.ID == "" || created.Name != "lab-alice" {
		t.Fatalf("create: unexpected payload: %s", raw)
	}

	// 2) read
	resp, raw = admin.do("GET", "/api/v1/groups/"+created.ID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get group: expected 200, got %d: %s", resp.StatusCode, raw)
	}
	var fetched struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(raw, &fetched)
	if fetched.Name != "lab-alice" {
		t.Fatalf("get: expected name=lab-alice, got %q", fetched.Name)
	}

	// 3) patch
	patchBody, _ := json.Marshal(map[string]string{
		"description": "Alice's research group (updated)",
	})
	resp, raw = admin.do("PATCH", "/api/v1/groups/"+created.ID, patchBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch group: expected 200, got %d: %s", resp.StatusCode, raw)
	}
	var patched struct {
		Description string `json:"description"`
	}
	_ = json.Unmarshal(raw, &patched)
	if patched.Description != "Alice's research group (updated)" {
		t.Fatalf("patch: unexpected description: %q", patched.Description)
	}

	// 4) delete
	resp, raw = admin.do("DELETE", "/api/v1/groups/"+created.ID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete group: expected 200, got %d: %s", resp.StatusCode, raw)
	}

	// 5) re-read should be 404
	resp, _ = admin.do("GET", "/api/v1/groups/"+created.ID, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: expected 404, got %d", resp.StatusCode)
	}
}

// TestGroupsCreateForbiddenForRegularUser verifies
// that a non-sysadmin user gets 403 on POST. The route
// is mounted behind AuthRequired, so we expect 401 if
// unauthenticated and 403 if authenticated-but-not-admin.
func TestGroupsCreateForbiddenForRegularUser(t *testing.T) {
	env := testutil.NewTestEnv(t)
	c := newAuthClient(env.BaseURL)
	r, _ := c.register("regular@example.com", "passw0rd!", "Reg")
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d", r.StatusCode)
	}
	c.token = loginUser(c, "regular@example.com", "passw0rd!")

	resp, _ := c.do("POST", "/api/v1/groups", []byte(`{"name":"x","description":"y"}`))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("create as regular user: expected 403, got %d", resp.StatusCode)
	}
}

// TestGroupsCreateDuplicateName covers the unique-name
// constraint. A second group with the same name
// returns 409 Conflict.
func TestGroupsCreateDuplicateName(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "dup-admin@example.com")

	body, _ := json.Marshal(map[string]string{"name": "unique-name", "description": "first"})
	resp, _ := admin.do("POST", "/api/v1/groups", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create: %d", resp.StatusCode)
	}
	body2, _ := json.Marshal(map[string]string{"name": "unique-name", "description": "second"})
	resp, raw := admin.do("POST", "/api/v1/groups", body2)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate create: expected 409, got %d: %s", resp.StatusCode, raw)
	}
}

// TestGroupsAddRemoveMember covers the membership
// lifecycle and verifies the corresponding casbin
// grouping policy is written. The check is direct:
// after AddMember, casbin_rule contains a row
// (g, userID, groupID, *). After RemoveMember, that
// row is gone.
func TestGroupsAddRemoveMember(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "members-admin@example.com")

	// Create a member user (no admin role).
	member := newAuthClient(env.BaseURL)
	mr, _ := member.register("member@example.com", "passw0rd!", "Member")
	if mr.StatusCode != http.StatusCreated {
		t.Fatalf("register member: %d", mr.StatusCode)
	}
	member.token = loginUser(member, "member@example.com", "passw0rd!")
	meResp, meRaw := member.do("GET", "/api/v1/users/me", nil)
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("fetch member: %d: %s", meResp.StatusCode, meRaw)
	}
	var me struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(meRaw, &me)

	// Create a group as admin.
	body, _ := json.Marshal(map[string]string{"name": "lab", "description": "lab group"})
	resp, raw := admin.do("POST", "/api/v1/groups", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create group: %d: %s", resp.StatusCode, raw)
	}
	var grp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &grp)

	// Add member.
	addBody, _ := json.Marshal(map[string]string{"user_id": me.ID})
	resp, addRaw := admin.do("POST", "/api/v1/groups/"+grp.ID+"/members", addBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("add member: expected 200, got %d: %s", resp.StatusCode, addRaw)
	}

	// Casbin row should exist.
	var exists bool
	if err := env.DB.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM casbin_rule WHERE p_type='g' AND v0=$1 AND v1=$2 AND v2='*')`,
		me.ID, grp.ID,
	).Scan(&exists); err != nil {
		t.Fatalf("query casbin: %v", err)
	}
	if !exists {
		t.Fatal("expected user→group casbin link after add, missing")
	}

	// List members should include the new user.
	resp, raw = admin.do("GET", "/api/v1/groups/"+grp.ID+"/members", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list members: %d: %s", resp.StatusCode, raw)
	}
	var listed struct {
		Members []struct {
			UserID string `json:"user_id"`
		} `json:"members"`
	}
	_ = json.Unmarshal(raw, &listed)
	if len(listed.Members) != 1 || listed.Members[0].UserID != me.ID {
		t.Fatalf("list members: unexpected %s", raw)
	}

	// Remove member.
	resp, rmRaw := admin.do("DELETE", "/api/v1/groups/"+grp.ID+"/members/"+me.ID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("remove member: expected 200, got %d: %s", resp.StatusCode, rmRaw)
	}
	if err := env.DB.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM casbin_rule WHERE p_type='g' AND v0=$1 AND v1=$2 AND v2='*')`,
		me.ID, grp.ID,
	).Scan(&exists); err != nil {
		t.Fatalf("query casbin post-remove: %v", err)
	}
	if exists {
		t.Fatal("expected user→group casbin link gone after remove, still present")
	}
}

// TestGroupsGrantRoleInheritedByMember is the central
// end-to-end test: a group with the editor role
// should grant a member the editor's permissions on
// the matching object. We assert via the
// `GET /api/v1/permissions` endpoint, which lists
// the caller's effective permissions. A regular
// user in an editor group should see source:update
// policies show up in their permission set.
func TestGroupsGrantRoleInheritedByMember(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "inherit-admin@example.com")

	// Create the group.
	body, _ := json.Marshal(map[string]string{"name": "editors", "description": "editors group"})
	resp, raw := admin.do("POST", "/api/v1/groups", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create group: %d: %s", resp.StatusCode, raw)
	}
	var grp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &grp)

	// Grant the editor role on the * domain.
	grantBody, _ := json.Marshal(map[string]string{
		"role":   rbac.RoleEditor,
		"domain": rbac.DomainAll,
	})
	resp, grantRaw := admin.do("PUT", "/api/v1/groups/"+grp.ID+"/roles", grantBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("grant role: expected 200, got %d: %s", resp.StatusCode, grantRaw)
	}

	// Create a regular user and add them to the group.
	user := newAuthClient(env.BaseURL)
	ur, _ := user.register("editor-via-group@example.com", "passw0rd!", "Editor")
	if ur.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d", ur.StatusCode)
	}
	user.token = loginUser(user, "editor-via-group@example.com", "passw0rd!")
	meResp, meRaw := user.do("GET", "/api/v1/users/me", nil)
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("fetch me: %d: %s", meResp.StatusCode, meRaw)
	}
	var me struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(meRaw, &me)

	addBody, _ := json.Marshal(map[string]string{"user_id": me.ID})
	resp, addRaw := admin.do("POST", "/api/v1/groups/"+grp.ID+"/members", addBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("add member: %d: %s", resp.StatusCode, addRaw)
	}
	// The chain (g, userID, groupID, *) and
	// (g, groupID, editor, *) needs to be live in
	// casbin's in-memory model. The AddMember call
	// writes both rows AND calls SavePolicy, which
	// re-derives the in-memory model. No explicit
	// reload is required.

	// Now hit /api/v1/permissions. The user's
	// effective permissions should include source
	// actions (editor grants source:*).
	permResp, permRaw := user.do("GET", "/api/v1/permissions", nil)
	if permResp.StatusCode != http.StatusOK {
		t.Fatalf("permissions: %d: %s", permResp.StatusCode, permRaw)
	}
	var perms struct {
		Permissions []struct {
			Resource string `json:"resource"`
			Action   string `json:"action"`
		} `json:"permissions"`
		SystemAdmin bool `json:"system_admin"`
	}
	_ = json.Unmarshal(permRaw, &perms)
	if perms.SystemAdmin {
		t.Fatalf("expected non-sysadmin, got sysadmin=true")
	}
	hasSourceWildcard := false
	for _, p := range perms.Permissions {
		if p.Resource == rbac.Objects.Sources && p.Action == rbac.Actions.Read {
			hasSourceWildcard = true
		}
	}
	if !hasSourceWildcard {
		t.Fatalf("expected source:read (editor's policy) via group-inherited editor role, got %s", permRaw)
	}
}

// TestGroupsRevokeRoleRevokesPermissions verifies
// that revoking a group's role removes the inherited
// permission from members. We use the same setup as
// the inherit test and assert that after revoke, the
// permission is gone.
func TestGroupsRevokeRoleRevokesPermissions(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "revoke-admin@example.com")

	body, _ := json.Marshal(map[string]string{"name": "revokable-editors", "description": ""})
	resp, raw := admin.do("POST", "/api/v1/groups", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create group: %d: %s", resp.StatusCode, raw)
	}
	var grp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &grp)

	grantBody, _ := json.Marshal(map[string]string{
		"role":   rbac.RoleEditor,
		"domain": rbac.DomainAll,
	})
	if r, _ := admin.do("PUT", "/api/v1/groups/"+grp.ID+"/roles", grantBody); r.StatusCode != http.StatusOK {
		t.Fatalf("grant role: %d", r.StatusCode)
	}

	user := newAuthClient(env.BaseURL)
	user.register("revoke-user@example.com", "passw0rd!", "Revoke")
	user.token = loginUser(user, "revoke-user@example.com", "passw0rd!")
	_, meRaw := user.do("GET", "/api/v1/users/me", nil)
	var me struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(meRaw, &me)

	addBody, _ := json.Marshal(map[string]string{"user_id": me.ID})
	admin.do("POST", "/api/v1/groups/"+grp.ID+"/members", addBody)

	// Confirm the user can see the editor's
	// source:read policy. The legacy user role does NOT
	// grant source permissions, so a `source:read` entry in
	// the permission list is a clean signal that the
	// group grant took effect.
	_, permRaw := user.do("GET", "/api/v1/permissions", nil)
	if !bytes.Contains(permRaw, []byte(`"resource":"source","action":"read"`)) {
		t.Fatalf("expected source:read pre-revoke, got %s", permRaw)
	}

	// Revoke.
	revokeBody, _ := json.Marshal(map[string]string{
		"role":   rbac.RoleEditor,
		"domain": rbac.DomainAll,
	})
	if r, _ := admin.do("DELETE", "/api/v1/groups/"+grp.ID+"/roles", revokeBody); r.StatusCode != http.StatusOK {
		t.Fatalf("revoke role: %d", r.StatusCode)
	}

	// Permissions should no longer include source:read.
	_, permRaw2 := user.do("GET", "/api/v1/permissions", nil)
	if bytes.Contains(permRaw2, []byte(`"resource":"source","action":"read"`)) {
		t.Fatalf("expected source:read gone post-revoke, got %s", permRaw2)
	}
}

// TestGroupsListUserGroupsSelf verifies the
// "what groups am I in?" view works for the regular
// user looking at their own memberships.
func TestGroupsListUserGroupsSelf(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "self-admin@example.com")

	body, _ := json.Marshal(map[string]string{"name": "self-test", "description": ""})
	resp, raw := admin.do("POST", "/api/v1/groups", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create group: %d: %s", resp.StatusCode, raw)
	}
	var grp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &grp)

	user := newAuthClient(env.BaseURL)
	user.register("self-user@example.com", "passw0rd!", "Self")
	user.token = loginUser(user, "self-user@example.com", "passw0rd!")
	_, meRaw := user.do("GET", "/api/v1/users/me", nil)
	var me struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(meRaw, &me)

	addBody, _ := json.Marshal(map[string]string{"user_id": me.ID})
	admin.do("POST", "/api/v1/groups/"+grp.ID+"/members", addBody)

	// Self-view: should see the group.
	resp, bodyRaw := user.do("GET", "/api/v1/users/"+me.ID+"/groups", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("self view: %d: %s", resp.StatusCode, bodyRaw)
	}
	var out struct {
		Groups []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"groups"`
	}
	_ = json.Unmarshal(bodyRaw, &out)
	if len(out.Groups) != 1 || out.Groups[0].ID != grp.ID {
		t.Fatalf("self view: expected one group, got %s", bodyRaw)
	}
}

// TestGroupsListUserGroupsOtherForbidden verifies that
// a regular user gets 403 when they try to look at
// somebody else's group memberships.
func TestGroupsListUserGroupsOtherForbidden(t *testing.T) {
	env := testutil.NewTestEnv(t)
	other := newAuthClient(env.BaseURL)
	other.register("other@example.com", "passw0rd!", "Other")
	other.token = loginUser(other, "other@example.com", "passw0rd!")
	_, otherMeRaw := other.do("GET", "/api/v1/users/me", nil)
	var otherMe struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(otherMeRaw, &otherMe)

	viewer := newAuthClient(env.BaseURL)
	viewer.register("viewer@example.com", "passw0rd!", "Viewer")
	viewer.token = loginUser(viewer, "viewer@example.com", "passw0rd!")

	resp, _ := viewer.do("GET", "/api/v1/users/"+otherMe.ID+"/groups", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("other view as regular user: expected 403, got %d", resp.StatusCode)
	}
}

// TestGroupsDeleteCleansCasbin verifies the
// DeleteGroup path also removes the user→group and
// group→role casbin links so the enforce path does
// not see a phantom group.
func TestGroupsDeleteCleansCasbin(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "del-groups-admin@example.com")

	body, _ := json.Marshal(map[string]string{"name": "to-delete", "description": ""})
	resp, raw := admin.do("POST", "/api/v1/groups", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d: %s", resp.StatusCode, raw)
	}
	var grp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &grp)

	// Add a member + a role.
	user := newAuthClient(env.BaseURL)
	user.register("del-user@example.com", "passw0rd!", "Del")
	user.token = loginUser(user, "del-user@example.com", "passw0rd!")
	_, meRaw := user.do("GET", "/api/v1/users/me", nil)
	var me struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(meRaw, &me)

	addBody, _ := json.Marshal(map[string]string{"user_id": me.ID})
	admin.do("POST", "/api/v1/groups/"+grp.ID+"/members", addBody)

	grantBody, _ := json.Marshal(map[string]string{
		"role":   rbac.RoleEditor,
		"domain": rbac.DomainAll,
	})
	admin.do("PUT", "/api/v1/groups/"+grp.ID+"/roles", grantBody)

	// Sanity: casbin rows exist.
	var n int
	if err := env.DB.QueryRow(context.Background(),
		`SELECT count(*) FROM casbin_rule WHERE p_type='g' AND v0=$1 AND v1=$2 AND v2='*'`,
		me.ID, grp.ID,
	).Scan(&n); err != nil {
		t.Fatalf("query user→group: %v", err)
	}
	if n == 0 {
		t.Fatal("expected user→group casbin row, got 0")
	}
	if err := env.DB.QueryRow(context.Background(),
		`SELECT count(*) FROM casbin_rule WHERE p_type='g' AND v0=$1 AND v1='editor' AND v2='*'`,
		grp.ID,
	).Scan(&n); err != nil {
		t.Fatalf("query group→role: %v", err)
	}
	if n == 0 {
		t.Fatal("expected group→editor casbin row, got 0")
	}

	// Delete the group.
	resp, delRaw := admin.do("DELETE", "/api/v1/groups/"+grp.ID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete: %d: %s", resp.StatusCode, delRaw)
	}

	// Both casbin rows should be gone.
	if err := env.DB.QueryRow(context.Background(),
		`SELECT count(*) FROM casbin_rule WHERE p_type='g' AND v0=$1 AND v1=$2 AND v2='*'`,
		me.ID, grp.ID,
	).Scan(&n); err != nil {
		t.Fatalf("query user→group post-delete: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected user→group casbin row gone, got %d", n)
	}
	if err := env.DB.QueryRow(context.Background(),
		`SELECT count(*) FROM casbin_rule WHERE p_type='g' AND v0=$1 AND v1='editor' AND v2='*'`,
		grp.ID,
	).Scan(&n); err != nil {
		t.Fatalf("query group→role post-delete: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected group→editor casbin row gone, got %d", n)
	}
}

// TestGroupsAuthRequired verifies that an unauthenticated
// request returns 401.
func TestGroupsAuthRequired(t *testing.T) {
	env := testutil.NewTestEnv(t)
	c := newAuthClient(env.BaseURL)
	resp, _ := c.do("GET", "/api/v1/groups", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth list: expected 401, got %d", resp.StatusCode)
	}
}
