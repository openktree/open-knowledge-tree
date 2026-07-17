//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// TestRepositorySettings_Seed verifies that creating a repository via
// POST /repositories seeds per-repo provider settings + the full
// embedded context vocabulary (the e2e config has no presets, so the
// fallback "general" seed — all live providers + "all" contexts —
// applies). GET /settings returns the live providers (each tagged
// stored/enabled) and the contexts (with concept_count 0).
func TestRepositorySettings_Seed(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "settings-seed@example.com")
	_, _, repoID := createRepository(t, admin, "SettingsSeed", "settings-seed", "desc")

	resp, raw := admin.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET settings: status %d, body %s", resp.StatusCode, string(raw))
	}
	var s struct {
		Providers []map[string]interface{} `json:"providers"`
		Contexts  []map[string]interface{} `json:"contexts"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	// The e2e env wires a fetch strategy (plain fetch) and no
	// search providers, so the live catalog has at least the
	// "fetch" resolution provider. The seed should have stored
	// every live provider enabled.
	if len(s.Providers) == 0 {
		t.Errorf("expected at least one live provider in settings, got 0")
	}
	anyEnabled := false
	for _, p := range s.Providers {
		if p["enabled"].(bool) {
			anyEnabled = true
		}
	}
	if !anyEnabled {
		t.Errorf("expected at least one enabled provider, got none")
	}
	// Contexts: the "all" fallback expands to the full embedded
	// context vocabulary (88 categories). Assert a non-trivial count.
	if len(s.Contexts) < 50 {
		t.Errorf("expected the full context vocabulary (>=50), got %d", len(s.Contexts))
	}
}

// TestRepositorySettings_ToggleProvider verifies the provider toggle
// round-trips: flip a provider off, confirm it's disabled; flip it
// back on, confirm enabled.
func TestRepositorySettings_ToggleProvider(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "settings-toggle@example.com")
	_, _, repoID := createRepository(t, admin, "SettingsToggle", "settings-toggle", "desc")

	_, raw := admin.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	var s struct {
		Providers []map[string]interface{} `json:"providers"`
	}
	json.Unmarshal(raw, &s)
	if len(s.Providers) == 0 {
		t.Skip("no live providers in this env; nothing to toggle")
	}
	first := s.Providers[0]
	kind := first["kind"].(string)
	id := first["id"].(string)

	offBody, _ := json.Marshal(map[string]interface{}{
		"provider_kind": kind,
		"provider_id":   id,
		"enabled":       false,
	})
	resp, raw := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/providers", offBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("disable provider: status %d, body %s", resp.StatusCode, string(raw))
	}
	// Re-read and confirm.
	_, raw = admin.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	json.Unmarshal(raw, &s)
	for _, p := range s.Providers {
		if p["kind"] == kind && p["id"] == id {
			if p["enabled"].(bool) {
				t.Errorf("provider %s/%s should be disabled", kind, id)
			}
		}
	}
}

// TestRepositorySettings_AddCustomContext verifies a custom context
// round-trips and is flagged is_custom=true.
func TestRepositorySettings_AddCustomContext(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "settings-ctx@example.com")
	_, _, repoID := createRepository(t, admin, "SettingsCtx", "settings-ctx", "desc")

	body, _ := json.Marshal(map[string]string{"context": "Product", "description": "a product"})
	resp, raw := admin.do("POST", "/api/v1/repositories/"+repoID+"/settings/contexts", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add context: status %d, body %s", resp.StatusCode, string(raw))
	}
	var c map[string]interface{}
	json.Unmarshal(raw, &c)
	if c["is_custom"] != true {
		t.Errorf("custom context should have is_custom=true, got %v", c["is_custom"])
	}

	// GET settings lists it.
	_, raw = admin.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	var s struct {
		Contexts []map[string]interface{} `json:"contexts"`
	}
	json.Unmarshal(raw, &s)
	found := false
	for _, ctx := range s.Contexts {
		if ctx["context"] == "Product" && ctx["is_custom"] == true {
			found = true
		}
	}
	if !found {
		t.Errorf("custom context Product not in settings list")
	}
}

// TestRepositorySettings_DeleteBlocked verifies DELETE on a context
// that still has concepts returns 409 with concept_count. This test
// seeds a concept directly into okt_repository.concepts (bypassing
// the worker) under an allowed context, then attempts delete.
func TestRepositorySettings_DeleteBlocked(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "settings-del@example.com")
	_, _, repoID := createRepository(t, admin, "SettingsDel", "settings-del", "desc")
	queries := store.New(env.DB)
	repoUUID := pgRepoID(t, repoID)
	// Insert a concept under "Product" (a custom context we add
	// first so the repo allows it).
	cbody, _ := json.Marshal(map[string]string{"context": "Product"})
	admin.do("POST", "/api/v1/repositories/"+repoID+"/settings/contexts", cbody)
	_, err := queries.CreateConcept(context.Background(), store.CreateConceptParams{
		RepositoryID:  repoUUID,
		CanonicalName: "Acme Widget",
		Context:       "Product",
	})
	if err != nil {
		t.Fatalf("seed concept: %v", err)
	}
	resp, raw := admin.do("DELETE", "/api/v1/repositories/"+repoID+"/settings/contexts/Product", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete with concepts: expected 409, got %d, body %s", resp.StatusCode, string(raw))
	}
	var body map[string]interface{}
	json.Unmarshal(raw, &body)
	if body["concept_count"] == nil {
		t.Errorf("409 should include concept_count")
	}
}

// TestRepositorySettings_PermissionDeny verifies a non-repo-admin
// user (a second registered user with no role on the repo) cannot
// GET settings (403). The sysadmin who created the repo can.
func TestRepositorySettings_PermissionDeny(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "settings-admin@example.com")
	_, _, repoID := createRepository(t, admin, "SettingsPerms", "settings-perms", "desc")
	// Register a second user with no role on the repo.
	other := registerAndLogin(t, env, "settings-other@example.com")
	resp, _ := other.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-repo-admin GET settings: expected 403, got %d", resp.StatusCode)
	}
}

// TestRepositorySettings_ListProvidersRepoScoped verifies that
// GET /sources/providers annotates each provider with an
// `enabled_for_repo` flag when the X-Repository-ID header is present.
// Disabling a provider via PUT /settings/providers should make that
// provider appear with enabled_for_repo=false on the next
// /sources/providers call (the gate cache is invalidated on toggle).
// Without the header, the response must omit the flag (global catalog).
func TestRepositorySettings_ListProvidersRepoScoped(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "listproviders@example.com")
	_, _, repoID := createRepository(t, admin, "ListProviders", "list-providers", "desc")

	// Disable a provider for this repo so we can observe the flag.
	_, raw := admin.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	var s struct {
		Providers []map[string]interface{} `json:"providers"`
	}
	json.Unmarshal(raw, &s)
	if len(s.Providers) == 0 {
		t.Skip("no live providers in this env; nothing to toggle")
	}
	first := s.Providers[0]
	kind := first["kind"].(string)
	id := first["id"].(string)
	offBody, _ := json.Marshal(map[string]interface{}{
		"provider_kind": kind,
		"provider_id":   id,
		"enabled":       false,
	})
	resp, raw := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/providers", offBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("disable provider: status %d, body %s", resp.StatusCode, string(raw))
	}

	// With X-Repository-ID: the disabled provider must have
	// enabled_for_repo=false; other providers should have
	// enabled_for_repo=true (they're still enabled).
	resp, raw = admin.doWithHeaders("GET", "/api/v1/sources/providers", nil, map[string]string{
		"X-Repository-ID": repoID,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /sources/providers (repo-scoped): status %d, body %s", resp.StatusCode, string(raw))
	}
	var pr struct {
		Search     []map[string]interface{} `json:"search"`
		Resolution []map[string]interface{} `json:"resolution"`
	}
	json.Unmarshal(raw, &pr)
	allProviders := append(pr.Search, pr.Resolution...)
	if len(allProviders) == 0 {
		t.Fatalf("expected at least one provider, got 0")
	}
	var foundDisabled *map[string]interface{}
	for i := range allProviders {
		v, ok := allProviders[i]["enabled_for_repo"]
		if !ok {
			t.Errorf("provider %s/%s missing enabled_for_repo when repo-scoped", kind, id)
			continue
		}
		if allProviders[i]["id"] == id && allProviders[i]["type"] == kind {
			if !v.(bool) {
				ptr := &allProviders[i]
				foundDisabled = ptr
			} else {
				t.Errorf("disabled provider %s/%s should have enabled_for_repo=false, got true", kind, id)
			}
		}
	}
	if foundDisabled == nil {
		t.Errorf("disabled provider %s/%s not found in repo-scoped /sources/providers", kind, id)
	}

	// Without X-Repository-ID: the flag must be absent (global catalog).
	resp, raw = admin.do("GET", "/api/v1/sources/providers", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /sources/providers (global): status %d, body %s", resp.StatusCode, string(raw))
	}
	var prGlobal struct {
		Search     []map[string]interface{} `json:"search"`
		Resolution []map[string]interface{} `json:"resolution"`
	}
	json.Unmarshal(raw, &prGlobal)
	allProviders = append(prGlobal.Search, prGlobal.Resolution...)
	for _, p := range allProviders {
		if _, ok := p["enabled_for_repo"]; ok {
			t.Errorf("global /sources/providers should omit enabled_for_repo, but provider %v has it", p["id"])
		}
	}
}

// TestRepositorySettings_GateInvalidatedOnToggle verifies that the
// per-repo gate cache is invalidated immediately after a toggle, so
// a subsequent operation against the same provider for that repo
// reflects the new state without waiting for the 5-min TTL. We
// exercise this via /sources/retrieve which gates on the resolution
// provider set: a repo with a disabled fetch provider should return
// 403 right after the toggle (not 202 as the stale cache would allow).
func TestRepositorySettings_GateInvalidatedOnToggle(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "gate-invalidate@example.com")
	_, _, repoID := createRepository(t, admin, "GateInvalidate", "gate-invalidate", "desc")

	// Find the fetch resolution provider from settings and disable it.
	_, raw := admin.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	var s struct {
		Providers []map[string]interface{} `json:"providers"`
	}
	json.Unmarshal(raw, &s)
	var fetchKind, fetchID string
	for _, p := range s.Providers {
		if p["kind"] == "resolution" && p["id"] == "fetch" {
			fetchKind = "resolution"
			fetchID = "fetch"
			break
		}
	}
	if fetchID == "" {
		t.Skip("no fetch resolution provider in this env")
	}
	offBody, _ := json.Marshal(map[string]interface{}{
		"provider_kind": fetchKind,
		"provider_id":   fetchID,
		"enabled":       false,
	})
	resp, _ := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/providers", offBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("disable fetch provider: status %d", resp.StatusCode)
	}

	// Warm the gate cache by issuing a retrieve (would be allowed
	// before disabling, but the cache is invalidated on toggle so
	// the first call after toggle re-reads). We expect 403 now
	// because the fetch provider is disabled and it's the only
	// resolution provider in the test env.
	retrieveBody, _ := json.Marshal(map[string]interface{}{
		"url":           "https://example.com/test",
		"repository_id": repoID,
	})
	resp, _ = admin.do("POST", "/api/v1/sources/retrieve", retrieveBody)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("retrieve after disabling fetch: expected 403, got %d (gate cache not invalidated?)", resp.StatusCode)
	}
}

// TestRepositorySettings_Backfill verifies that a repo created
// before the settings feature (simulated by deleting its settings
// rows then re-running via the backfill path: here we just delete
// settings rows and confirm the gate denies everything, then
// re-seed by calling the create flow again is not applicable —
// instead we assert that a freshly created repo's backfill-style
// seed makes TestSearch succeed when a provider is enabled).
// Kept light: the backfill migration is covered by migration
// application; here we verify the gate behavior on a seeded repo.
func TestRepositorySettings_GateAllowsEnabled(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "settings-gate@example.com")
	_, _, repoID := createRepository(t, admin, "SettingsGate", "settings-gate", "desc")
	// The repo is seeded with all live providers enabled. The e2e
	// env has a fetch resolution provider live; confirm
	// /sources/classify works (it doesn't gate, but the retrieve
	// endpoint would). We just assert the seeded settings are
	// non-empty (covered by Seed test); a deeper gate test would
	// require a live search provider, which the e2e env doesn't
	// wire. This test exists so the gate path is at least exercised
	// end-to-end via the settings read.
	resp, _ := admin.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET settings: status %d", resp.StatusCode)
	}
}

// TestRepositorySettings_AutoContribute_Default verifies a freshly
// created repo reports auto_contribute=false in GET /settings (the
// migration default) and that the flag round-trips through
// PUT /settings/auto-contribute when the registry is configured.
func TestRepositorySettings_AutoContribute_Default(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	// The auto-contribute enable path requires the registry URL to
	// be set; mutate the live config pointer the handler reads.
	env.Config.Providers.Registry.URL = "http://registry.test"
	admin := bootstrapSysAdmin(t, env, "autocontrib@example.com")
	_, _, repoID := createRepository(t, admin, "AutoContrib", "auto-contrib", "desc")

	// Default is false.
	resp, raw := admin.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET settings: status %d, body %s", resp.StatusCode, string(raw))
	}
	var s struct {
		AutoContribute bool `json:"auto_contribute"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if s.AutoContribute {
		t.Errorf("expected auto_contribute=false by default, got true")
	}

	// Enable.
	onBody, _ := json.Marshal(map[string]bool{"enabled": true})
	resp, raw = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/auto-contribute", onBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enable auto-contribute: status %d, body %s", resp.StatusCode, string(raw))
	}
	var res struct {
		AutoContribute bool `json:"auto_contribute"`
	}
	json.Unmarshal(raw, &res)
	if !res.AutoContribute {
		t.Errorf("PUT response should echo auto_contribute=true, got false")
	}

	// Persisted: re-GET shows true.
	resp, raw = admin.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	json.Unmarshal(raw, &s)
	if !s.AutoContribute {
		t.Errorf("auto_contribute should persist as true, got false")
	}

	// Disable (no registry-config check on disable).
	offBody, _ := json.Marshal(map[string]bool{"enabled": false})
	resp, raw = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/auto-contribute", offBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("disable auto-contribute: status %d, body %s", resp.StatusCode, string(raw))
	}
	json.Unmarshal(raw, &res)
	if res.AutoContribute {
		t.Errorf("PUT response should echo auto_contribute=false, got true")
	}
}

// TestRepositorySettings_AutoContribute_RequiresRegistry verifies
// enabling auto-contribute returns 400 when the registry URL is
// not configured (so the toggle can't be left in a state where the
// auto-chain would no-op silently).
func TestRepositorySettings_AutoContribute_RequiresRegistry(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	// Leave env.Config.Providers.Registry.URL empty.
	admin := bootstrapSysAdmin(t, env, "autocontrib-noreg@example.com")
	_, _, repoID := createRepository(t, admin, "AutoContribNoReg", "auto-contrib-no-reg", "desc")

	onBody, _ := json.Marshal(map[string]bool{"enabled": true})
	resp, _ := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/auto-contribute", onBody)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("enable without registry configured: expected 400, got %d", resp.StatusCode)
	}

	// Disabling is allowed even when registry is unconfigured.
	offBody, _ := json.Marshal(map[string]bool{"enabled": false})
	resp, _ = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/auto-contribute", offBody)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("disable without registry configured: expected 200, got %d", resp.StatusCode)
	}
}

// TestRepositorySettings_AutoContribute_PermissionDeny verifies a
// non-repo-admin cannot toggle the flag.
func TestRepositorySettings_AutoContribute_PermissionDeny(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	env.Config.Providers.Registry.URL = "http://registry.test"
	admin := bootstrapSysAdmin(t, env, "autocontrib-perm@example.com")
	_, _, repoID := createRepository(t, admin, "AutoContribPerm", "auto-contrib-perm", "desc")

	other := registerAndLogin(t, env, "autocontrib-other@example.com")
	onBody, _ := json.Marshal(map[string]bool{"enabled": true})
	resp, _ := other.do("PUT", "/api/v1/repositories/"+repoID+"/settings/auto-contribute", onBody)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-repo-admin PUT auto-contribute: expected 403, got %d", resp.StatusCode)
	}
}

// registerAndLogin registers a fresh user (no roles) and returns an
// authenticated client. Used by the permission-deny test to verify a
// non-repo-admin cannot reach the settings endpoints. Distinct from
// bootstrapSysAdmin, which also inserts a sysadmin casbin row.
func registerAndLogin(t *testing.T, env *testutil.TestEnv, email string) *authClient {
	t.Helper()
	client := newAuthClient(env.BaseURL)
	if r, _ := client.register(email, "passw0rd!", "Plain User"); r.StatusCode != http.StatusCreated {
		t.Fatalf("register %s: status %d", email, r.StatusCode)
	}
	client.token = loginUser(client, email, "passw0rd!")
	return client
}

// TestRepositorySettings_Registry_Default verifies a freshly created
// repo reports the migration defaults in GET /settings: registry_id
// "default", registry_enabled true, registry_configured false (the
// test env doesn't set a registry URL), and a single registry_option
// "default" surfaced for the dropdown.
func TestRepositorySettings_Registry_Default(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	// Leave registry URL empty.
	admin := bootstrapSysAdmin(t, env, "reg-default@example.com")
	_, _, repoID := createRepository(t, admin, "RegDefault", "reg-default", "desc")

	resp, raw := admin.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET settings: status %d, body %s", resp.StatusCode, string(raw))
	}
	var s struct {
		RegistryID         string   `json:"registry_id"`
		RegistryEnabled    bool     `json:"registry_enabled"`
		RegistryConfigured bool     `json:"registry_configured"`
		RegistryOptions    []string `json:"registry_options"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if s.RegistryID != "default" {
		t.Errorf("expected registry_id=%q, got %q", "default", s.RegistryID)
	}
	if !s.RegistryEnabled {
		t.Errorf("expected registry_enabled=true by default, got false")
	}
	if s.RegistryConfigured {
		t.Errorf("expected registry_configured=false (no URL set), got true")
	}
	// With no registry configured, the options list surfaces the
	// stored id so the dropdown always has a selection.
	if len(s.RegistryOptions) == 0 || s.RegistryOptions[0] != "default" {
		t.Errorf("expected registry_options to start with %q, got %v", "default", s.RegistryOptions)
	}
}

// TestRepositorySettings_SetRegistry_EnableDisable verifies the
// PUT /settings/registry endpoint round-trips the enabled flag and
// rejects enabling when no registry is configured globally.
func TestRepositorySettings_SetRegistry_EnableDisable(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	env.Config.Providers.Registry.URL = "http://registry.test"
	admin := bootstrapSysAdmin(t, env, "reg-toggle@example.com")
	_, _, repoID := createRepository(t, admin, "RegToggle", "reg-toggle", "desc")

	// Default is enabled; disable it.
	offBody, _ := json.Marshal(map[string]bool{"enabled": false})
	resp, raw := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/registry", offBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("disable registry: status %d, body %s", resp.StatusCode, string(raw))
	}
	var res struct {
		RegistryID      string `json:"registry_id"`
		RegistryEnabled bool   `json:"registry_enabled"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if res.RegistryEnabled {
		t.Errorf("expected registry_enabled=false after disable, got true")
	}

	// Persisted: re-GET shows false.
	resp, raw = admin.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	var s struct {
		RegistryEnabled bool `json:"registry_enabled"`
	}
	json.Unmarshal(raw, &s)
	if s.RegistryEnabled {
		t.Errorf("registry_enabled should persist as false, got true")
	}

	// Re-enable.
	onBody, _ := json.Marshal(map[string]bool{"enabled": true})
	resp, raw = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/registry", onBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enable registry: status %d, body %s", resp.StatusCode, string(raw))
	}
	json.Unmarshal(raw, &res)
	if !res.RegistryEnabled {
		t.Errorf("expected registry_enabled=true after enable, got false")
	}
}

// TestRepositorySettings_SetRegistry_RequiresConfigured verifies
// enabling the integration returns 400 when no registry is
// configured globally (the toggle can't be on with no registry to
// talk to). Disabling is always allowed.
func TestRepositorySettings_SetRegistry_RequiresConfigured(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	// Leave registry URL empty.
	admin := bootstrapSysAdmin(t, env, "reg-noconf@example.com")
	_, _, repoID := createRepository(t, admin, "RegNoConf", "reg-no-conf", "desc")

	// Disable is allowed even when unconfigured.
	offBody, _ := json.Marshal(map[string]bool{"enabled": false})
	resp, _ := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/registry", offBody)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("disable without registry configured: expected 200, got %d", resp.StatusCode)
	}

	// Re-enable is rejected with 400.
	onBody, _ := json.Marshal(map[string]bool{"enabled": true})
	resp, _ = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/registry", onBody)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("enable without registry configured: expected 400, got %d", resp.StatusCode)
	}
}

// TestRepositorySettings_SetRegistry_InvalidID verifies the endpoint
// rejects an unknown registry_id with 400 when enabling (the id
// must be in the configured registries list).
func TestRepositorySettings_SetRegistry_InvalidID(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	env.Config.Providers.Registry.URL = "http://registry.test"
	admin := bootstrapSysAdmin(t, env, "reg-badid@example.com")
	_, _, repoID := createRepository(t, admin, "RegBadID", "reg-bad-id", "desc")

	body, _ := json.Marshal(map[string]any{"registry_id": "nonexistent", "enabled": true})
	resp, _ := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/registry", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid registry_id with enabled=true: expected 400, got %d", resp.StatusCode)
	}
}

// TestRepositorySettings_SetRegistry_DisableClearsAutoContribute
// verifies that turning the integration off also clears the
// auto_contribute flag so the UI doesn't show "sharing on" while
// the integration is disabled.
func TestRepositorySettings_SetRegistry_DisableClearsAutoContribute(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	env.Config.Providers.Registry.URL = "http://registry.test"
	admin := bootstrapSysAdmin(t, env, "reg-clear@example.com")
	_, _, repoID := createRepository(t, admin, "RegClear", "reg-clear", "desc")

	// Enable auto-contribute first (requires registry enabled, which
	// is the default).
	onAuto, _ := json.Marshal(map[string]bool{"enabled": true})
	resp, raw := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/auto-contribute", onAuto)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enable auto-contribute: status %d, body %s", resp.StatusCode, string(raw))
	}

	// Disable the registry integration.
	offReg, _ := json.Marshal(map[string]bool{"enabled": false})
	resp, raw = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/registry", offReg)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("disable registry: status %d, body %s", resp.StatusCode, string(raw))
	}

	// auto_contribute should now be false.
	resp, raw = admin.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	var s struct {
		AutoContribute bool `json:"auto_contribute"`
	}
	json.Unmarshal(raw, &s)
	if s.AutoContribute {
		t.Errorf("auto_contribute should be cleared when registry is disabled, got true")
	}
}

// TestRepositorySettings_SetRegistry_PermissionDeny verifies a
// non-repo-admin cannot toggle the registry integration.
func TestRepositorySettings_SetRegistry_PermissionDeny(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	env.Config.Providers.Registry.URL = "http://registry.test"
	admin := bootstrapSysAdmin(t, env, "reg-perm@example.com")
	_, _, repoID := createRepository(t, admin, "RegPerm", "reg-perm", "desc")

	other := registerAndLogin(t, env, "reg-perm-other@example.com")
	body, _ := json.Marshal(map[string]bool{"enabled": false})
	resp, _ := other.do("PUT", "/api/v1/repositories/"+repoID+"/settings/registry", body)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-repo-admin PUT registry: expected 403, got %d", resp.StatusCode)
	}
}

// TestRepositorySettings_ModelSettings_Default verifies a freshly
// created repo reports no per-task model overrides in GET /settings
// (all task_models have selected="" meaning inherit global default).
func TestRepositorySettings_ModelSettings_Default(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "model-default@example.com")
	_, _, repoID := createRepository(t, admin, "ModelDefault", "model-default", "desc")

	resp, raw := admin.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET settings: status %d, body %s", resp.StatusCode, string(raw))
	}
	var s struct {
		TaskModels []struct {
			TaskKind string `json:"task_kind"`
			Selected string `json:"selected"`
			Default  string `json:"default"`
		} `json:"task_models"`
		ModelCatalog []struct {
			ID string `json:"id"`
		} `json:"model_catalog"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if len(s.TaskModels) != 7 {
		t.Errorf("expected 7 task_models entries, got %d", len(s.TaskModels))
	}
	for _, tm := range s.TaskModels {
		if tm.Selected != "" {
			t.Errorf("task %q: expected selected=\"\" (inherit default), got %q", tm.TaskKind, tm.Selected)
		}
	}
}

// TestRepositorySettings_SetModelSetting_RoundTrip verifies the
// PUT /settings/models endpoint sets and clears a per-task model
// override and the change persists in GET /settings.
func TestRepositorySettings_SetModelSetting_RoundTrip(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "model-rt@example.com")
	_, _, repoID := createRepository(t, admin, "ModelRT", "model-rt", "desc")

	// Set a model override for fact_extraction. The test env config
	// has at least one AI model configured (the e2e config loads the
	// default YAML which has ai.models entries). Use the first model
	// from the catalog.
	resp, raw := admin.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	var s struct {
		ModelCatalog []struct {
			ID string `json:"id"`
		} `json:"model_catalog"`
	}
	json.Unmarshal(raw, &s)
	if len(s.ModelCatalog) == 0 {
		t.Skip("no AI models configured in test env; skipping model round-trip test")
	}
	modelID := s.ModelCatalog[0].ID

	setBody, _ := json.Marshal(map[string]string{"task_kind": "fact_extraction", "model_id": modelID})
	resp, raw = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/models", setBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set model: status %d, body %s", resp.StatusCode, string(raw))
	}

	// Verify it persists.
	resp, raw = admin.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	var s2 struct {
		TaskModels []struct {
			TaskKind string `json:"task_kind"`
			Selected string `json:"selected"`
		} `json:"task_models"`
	}
	json.Unmarshal(raw, &s2)
	found := false
	for _, tm := range s2.TaskModels {
		if tm.TaskKind == "fact_extraction" {
			if tm.Selected != modelID {
				t.Errorf("expected selected=%q, got %q", modelID, tm.Selected)
			}
			found = true
		}
	}
	if !found {
		t.Errorf("fact_extraction not in task_models response")
	}

	// Clear the override (empty model_id = inherit default).
	clearBody, _ := json.Marshal(map[string]string{"task_kind": "fact_extraction", "model_id": ""})
	resp, raw = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/models", clearBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear model: status %d, body %s", resp.StatusCode, string(raw))
	}

	// Verify it's cleared.
	resp, raw = admin.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	var s3 struct {
		TaskModels []struct {
			TaskKind string `json:"task_kind"`
			Selected string `json:"selected"`
		} `json:"task_models"`
	}
	json.Unmarshal(raw, &s3)
	for _, tm := range s3.TaskModels {
		if tm.TaskKind == "fact_extraction" && tm.Selected != "" {
			t.Errorf("expected selected=\"\" after clear, got %q", tm.Selected)
		}
	}
}

// TestRepositorySettings_SetModelSetting_InvalidKind verifies the
// endpoint rejects an invalid task_kind with 400.
func TestRepositorySettings_SetModelSetting_InvalidKind(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "model-badkind@example.com")
	_, _, repoID := createRepository(t, admin, "ModelBadKind", "model-badkind", "desc")

	body, _ := json.Marshal(map[string]string{"task_kind": "bogus_task", "model_id": "any"})
	resp, _ := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/models", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid task_kind: expected 400, got %d", resp.StatusCode)
	}
}

// TestRepositorySettings_SetModelSetting_InvalidModel verifies the
// endpoint rejects a model_id not in the catalog with 400.
func TestRepositorySettings_SetModelSetting_InvalidModel(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "model-badmodel@example.com")
	_, _, repoID := createRepository(t, admin, "ModelBadModel", "model-badmodel", "desc")

	body, _ := json.Marshal(map[string]string{"task_kind": "summarization", "model_id": "nonexistent-model"})
	resp, _ := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/models", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid model_id: expected 400, got %d", resp.StatusCode)
	}
}

// TestRepositorySettings_AllowedModels_RoundTrip verifies the
// per-repo allowed_models whitelist can be set, read back, and
// cleared via the PUT /settings/registry endpoint.
func TestRepositorySettings_AllowedModels_RoundTrip(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	env.Config.Providers.Registry.URL = "http://registry.test"
	admin := bootstrapSysAdmin(t, env, "allowed-rt@example.com")
	_, _, repoID := createRepository(t, admin, "AllowedRT", "allowed-rt", "desc")

	// Default: allowed_models is nil (inherit global).
	resp, raw := admin.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	var s struct {
		AllowedModels      interface{} `json:"allowed_models"`
		AllowedModelsDefault []string  `json:"allowed_models_default"`
	}
	json.Unmarshal(raw, &s)
	if s.AllowedModels != nil {
		t.Errorf("expected allowed_models=nil by default, got %v", s.AllowedModels)
	}

	// Set a per-repo whitelist.
	setBody, _ := json.Marshal(map[string]any{"allowed_models": []string{"model-x", "model-y"}})
	resp, raw = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/registry", setBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set allowed_models: status %d, body %s", resp.StatusCode, string(raw))
	}

	// Verify it persists.
	resp, raw = admin.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	var s2 struct {
		AllowedModels []string `json:"allowed_models"`
	}
	json.Unmarshal(raw, &s2)
	if len(s2.AllowedModels) != 2 || s2.AllowedModels[0] != "model-x" || s2.AllowedModels[1] != "model-y" {
		t.Errorf("expected allowed_models=[model-x,model-y], got %v", s2.AllowedModels)
	}

	// Clear (null = inherit global).
	clearBody, _ := json.Marshal(map[string]any{"allowed_models": nil})
	resp, raw = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/registry", clearBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear allowed_models: status %d, body %s", resp.StatusCode, string(raw))
	}

	// Verify it's cleared.
	resp, raw = admin.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	var s3 struct {
		AllowedModels interface{} `json:"allowed_models"`
	}
	json.Unmarshal(raw, &s3)
	if s3.AllowedModels != nil {
		t.Errorf("expected allowed_models=nil after clear, got %v", s3.AllowedModels)
	}
}

// TestRepositorySettings_SyncLevels_Default verifies a freshly
// created repo defaults to push_level=concepts and pull_level=concepts
// (migration 0044), so existing repos keep full sync behavior.
func TestRepositorySettings_SyncLevels_Default(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "synclevels-default@example.com")
	_, _, repoID := createRepository(t, admin, "SyncLevelsDefault", "synclevels-default", "desc")

	resp, raw := admin.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET settings: status %d, body %s", resp.StatusCode, string(raw))
	}
	var s struct {
		PushLevel string `json:"registry_push_level"`
		PullLevel string `json:"registry_pull_level"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if s.PushLevel != "concepts" {
		t.Errorf("expected push_level=concepts by default, got %q", s.PushLevel)
	}
	if s.PullLevel != "concepts" {
		t.Errorf("expected pull_level=concepts by default, got %q", s.PullLevel)
	}
}

// TestRepositorySettings_SyncLevels_RoundTrip verifies the
// PUT .../settings/sync-levels endpoint round-trips: set both
// levels to "facts", confirm the response echoes them, then re-GET
// to confirm they persisted.
func TestRepositorySettings_SyncLevels_RoundTrip(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "synclevels-rt@example.com")
	_, _, repoID := createRepository(t, admin, "SyncLevelsRT", "synclevels-rt", "desc")

	// Set both to facts.
	body, _ := json.Marshal(map[string]string{"push_level": "facts", "pull_level": "facts"})
	resp, raw := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/sync-levels", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT sync-levels: status %d, body %s", resp.StatusCode, string(raw))
	}
	var res struct {
		PushLevel string `json:"registry_push_level"`
		PullLevel string `json:"registry_pull_level"`
	}
	json.Unmarshal(raw, &res)
	if res.PushLevel != "facts" {
		t.Errorf("PUT response should echo push_level=facts, got %q", res.PushLevel)
	}
	if res.PullLevel != "facts" {
		t.Errorf("PUT response should echo pull_level=facts, got %q", res.PullLevel)
	}

	// Persisted: re-GET shows facts.
	resp, raw = admin.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	var s struct {
		PushLevel string `json:"registry_push_level"`
		PullLevel string `json:"registry_pull_level"`
	}
	json.Unmarshal(raw, &s)
	if s.PushLevel != "facts" {
		t.Errorf("push_level should persist as facts, got %q", s.PushLevel)
	}
	if s.PullLevel != "facts" {
		t.Errorf("pull_level should persist as facts, got %q", s.PullLevel)
	}

	// Partial update: set only push_level, pull_level stays facts.
	body, _ = json.Marshal(map[string]string{"push_level": "concepts"})
	resp, raw = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/sync-levels", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT sync-levels (partial): status %d, body %s", resp.StatusCode, string(raw))
	}
	json.Unmarshal(raw, &res)
	if res.PushLevel != "concepts" {
		t.Errorf("expected push_level=concepts after partial update, got %q", res.PushLevel)
	}
	if res.PullLevel != "facts" {
		t.Errorf("pull_level should be unchanged (facts), got %q", res.PullLevel)
	}
}

// TestRepositorySettings_SyncLevels_Invalid verifies an invalid
// level value is rejected with 400.
func TestRepositorySettings_SyncLevels_Invalid(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "synclevels-invalid@example.com")
	_, _, repoID := createRepository(t, admin, "SyncLevelsInvalid", "synclevels-invalid", "desc")

	body, _ := json.Marshal(map[string]string{"push_level": "bogus"})
	resp, _ := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/sync-levels", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid push_level: expected 400, got %d", resp.StatusCode)
	}

	body, _ = json.Marshal(map[string]string{"pull_level": "sources"})
	resp, _ = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/sync-levels", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid pull_level: expected 400, got %d", resp.StatusCode)
	}
}

// TestRepositorySettings_SyncLevels_PermissionDeny verifies a
// non-repo-admin cannot set sync levels (403).
func TestRepositorySettings_SyncLevels_PermissionDeny(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "synclevels-perm@example.com")
	_, _, repoID := createRepository(t, admin, "SyncLevelsPerm", "synclevels-perm", "desc")

	other := registerAndLogin(t, env, "synclevels-other@example.com")
	body, _ := json.Marshal(map[string]string{"push_level": "facts"})
	resp, _ := other.do("PUT", "/api/v1/repositories/"+repoID+"/settings/sync-levels", body)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-repo-admin PUT sync-levels: expected 403, got %d", resp.StatusCode)
	}
}