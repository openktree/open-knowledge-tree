//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/openktree/open-knowledge-tree/backend/internal/taskmanager/tasks"
)

// TestContextMapping_CRUD verifies the context-mapping endpoints
// round-trip: upsert a mapping, list it, delete it. Also verifies
// the 400 paths (local_context not in repository_contexts) and the
// cascade delete (deleting a context drops its mapping).
func TestContextMapping_CRUD(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "ctxmap-crud@example.com")
	_, _, repoID := createRepository(t, admin, "CtxMapCRUD", "ctxmap-crud", "desc")

	// Add a custom local context "Product" so we can map it.
	cbody, _ := json.Marshal(map[string]string{"context": "Product", "description": "a product"})
	admin.do("POST", "/api/v1/repositories/"+repoID+"/settings/contexts", cbody)

	// Upsert a mapping Product → Politician (a standard label that
	// is also in the registry vocab when the registry is configured;
	// here the registry is unconfigured so validation is off and any
	// non-empty target is accepted).
	mapBody, _ := json.Marshal(map[string]string{"local_context": "Product", "registry_context": "Politician"})
	resp, raw := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/context-mappings", mapBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upsert mapping: status %d, body %s", resp.StatusCode, string(raw))
	}
	var m map[string]interface{}
	json.Unmarshal(raw, &m)
	if m["local_context"] != "Product" || m["registry_context"] != "Politician" {
		t.Errorf("upsert response = %+v, want local=Product registry=Politician", m)
	}

	// List mappings via the dedicated endpoint.
	resp, raw = admin.do("GET", "/api/v1/repositories/"+repoID+"/settings/context-mappings", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list mappings: status %d, body %s", resp.StatusCode, string(raw))
	}
	var list struct {
		Mappings []map[string]interface{} `json:"mappings"`
	}
	json.Unmarshal(raw, &list)
	found := false
	for _, mm := range list.Mappings {
		if mm["local_context"] == "Product" && mm["registry_context"] == "Politician" {
			found = true
		}
	}
	if !found {
		t.Errorf("mapping Product→Politician not in list: %+v", list.Mappings)
	}

	// GET /settings also surfaces the mapping (page-load view).
	resp, raw = admin.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET settings: status %d, body %s", resp.StatusCode, string(raw))
	}
	var s struct {
		ContextMappings []map[string]interface{} `json:"context_mappings"`
		UnmappedPolicy  string                   `json:"unmapped_policy"`
		UnmappedLocal   []string                 `json:"unmapped_local"`
	}
	json.Unmarshal(raw, &s)
	found = false
	for _, mm := range s.ContextMappings {
		if mm["local_context"] == "Product" {
			found = true
		}
	}
	if !found {
		t.Errorf("mapping not surfaced in GET /settings: %+v", s.ContextMappings)
	}
	if s.UnmappedPolicy != "skip" {
		t.Errorf("default unmapped_policy = %q, want skip", s.UnmappedPolicy)
	}

	// Delete the mapping via the dedicated endpoint.
	resp, _ = admin.do("DELETE", "/api/v1/repositories/"+repoID+"/settings/context-mappings/Product", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("delete mapping: expected 200, got %d", resp.StatusCode)
	}

	// 404 on re-delete.
	resp, _ = admin.do("DELETE", "/api/v1/repositories/"+repoID+"/settings/context-mappings/Product", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("re-delete mapping: expected 404, got %d", resp.StatusCode)
	}

	// 400 when local_context is not in repository_contexts.
	badBody, _ := json.Marshal(map[string]string{"local_context": "Nonexistent", "registry_context": "Politician"})
	resp, _ = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/context-mappings", badBody)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("upsert with unknown local_context: expected 400, got %d", resp.StatusCode)
	}

	// Cascade: re-add the mapping, then delete the context and
	// confirm the mapping is gone.
	admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/context-mappings", mapBody)
	// Delete the context (it has no concepts so it succeeds).
	admin.do("DELETE", "/api/v1/repositories/"+repoID+"/settings/contexts/Product", nil)
	resp, raw = admin.do("GET", "/api/v1/repositories/"+repoID+"/settings/context-mappings", nil)
	json.Unmarshal(raw, &list)
	for _, mm := range list.Mappings {
		if mm["local_context"] == "Product" {
			t.Errorf("mapping for Product should have been cascade-deleted with the context")
		}
	}
}

// TestContextMapping_RegistryVocab verifies that when a stub registry
// is wired and exposes GET /api/v1/contexts, the settings endpoint
// surfaces the registry's vocab in registry_contexts, and the upsert
// rejects a registry_context not in the vocab (400). Also verifies
// the unmapped_local computation: a local context with no mapping
// whose label is NOT in the registry vocab appears in unmapped_local.
func TestContextMapping_RegistryVocab(t *testing.T) {
	stubRegistry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/contexts" {
			_, _ = w.Write([]byte(`{"contexts":["Politician","Organization","Place"]}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(stubRegistry.Close)

	env, _, _, _ := newRemoteEnvWithRegistry(t, stubRegistry.URL)
	admin := bootstrapSysAdmin(t, env, "ctxmap-vocab@example.com")
	_, _, repoID := createRepository(t, admin, "CtxMapVocab", "ctxmap-vocab", "desc")

	// Add a custom local context "Product" (not in the registry
	// vocab) so it should appear in unmapped_local.
	cbody, _ := json.Marshal(map[string]string{"context": "Product"})
	admin.do("POST", "/api/v1/repositories/"+repoID+"/settings/contexts", cbody)

	resp, raw := admin.do("GET", "/api/v1/repositories/"+repoID+"/settings/context-mappings", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list mappings: status %d, body %s", resp.StatusCode, string(raw))
	}
	var s struct {
		RegistryContexts []string `json:"registry_contexts"`
		UnmappedLocal    []string `json:"unmapped_local"`
	}
	json.Unmarshal(raw, &s)
	// The stub registry publishes 3 contexts.
	if len(s.RegistryContexts) != 3 {
		t.Errorf("registry_contexts = %v, want 3 labels", s.RegistryContexts)
	}
	// "Product" is a local context, has no mapping, and is not in
	// the registry vocab → it should be in unmapped_local.
	foundProduct := false
	for _, u := range s.UnmappedLocal {
		if u == "Product" {
			foundProduct = true
		}
	}
	if !foundProduct {
		t.Errorf("Product should be in unmapped_local, got %v", s.UnmappedLocal)
	}

	// Upsert with a valid registry target succeeds.
	goodBody, _ := json.Marshal(map[string]string{"local_context": "Product", "registry_context": "Politician"})
	resp, _ = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/context-mappings", goodBody)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("upsert with valid registry_context: expected 200, got %d", resp.StatusCode)
	}

	// Upsert with an invalid registry target fails (the vocab is
	// non-empty so validation is on).
	badBody, _ := json.Marshal(map[string]string{"local_context": "Product", "registry_context": "PhantomLabel"})
	resp, _ = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/context-mappings", badBody)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("upsert with phantom registry_context: expected 400, got %d", resp.StatusCode)
	}
}

// TestContextMapping_UnmappedPolicy verifies the unmapped-policy
// endpoint round-trips and validates the catch_all_context.
func TestContextMapping_UnmappedPolicy(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "ctxmap-policy@example.com")
	_, _, repoID := createRepository(t, admin, "CtxMapPolicy", "ctxmap-policy", "desc")

	// Add a custom context "Other" to use as catch_all.
	cbody, _ := json.Marshal(map[string]string{"context": "Other"})
	admin.do("POST", "/api/v1/repositories/"+repoID+"/settings/contexts", cbody)

	// Set policy to catch_all with a valid catch_all_context.
	goodBody, _ := json.Marshal(map[string]interface{}{"policy": "catch_all", "catch_all_context": "Other"})
	resp, raw := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/unmapped-policy", goodBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set catch_all policy: status %d, body %s", resp.StatusCode, string(raw))
	}
	var res struct {
		UnmappedPolicy string `json:"unmapped_policy"`
	}
	json.Unmarshal(raw, &res)
	if res.UnmappedPolicy != "catch_all" {
		t.Errorf("response policy = %q, want catch_all", res.UnmappedPolicy)
	}

	// Persisted: GET /settings shows the policy.
	resp, raw = admin.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	var s struct {
		UnmappedPolicy string `json:"unmapped_policy"`
	}
	json.Unmarshal(raw, &s)
	if s.UnmappedPolicy != "catch_all" {
		t.Errorf("persisted policy = %q, want catch_all", s.UnmappedPolicy)
	}

	// catch_all with an invalid catch_all_context fails.
	badBody, _ := json.Marshal(map[string]interface{}{"policy": "catch_all", "catch_all_context": "Nonexistent"})
	resp, _ = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/unmapped-policy", badBody)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("catch_all with invalid context: expected 400, got %d", resp.StatusCode)
	}

	// catch_all without a catch_all_context fails.
	noCtx, _ := json.Marshal(map[string]interface{}{"policy": "catch_all"})
	resp, _ = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/unmapped-policy", noCtx)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("catch_all without context: expected 400, got %d", resp.StatusCode)
	}

	// Invalid policy fails.
	badPolicy, _ := json.Marshal(map[string]interface{}{"policy": "bogus"})
	resp, _ = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/unmapped-policy", badPolicy)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid policy: expected 400, got %d", resp.StatusCode)
	}

	// Set back to skip.
	skipBody, _ := json.Marshal(map[string]interface{}{"policy": "skip"})
	resp, _ = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/unmapped-policy", skipBody)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("set skip policy: expected 200, got %d", resp.StatusCode)
	}
}

// TestContextMapping_PermissionDeny verifies a non-repo-admin
// cannot reach the context-mapping endpoints.
func TestContextMapping_PermissionDeny(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "ctxmap-perm@example.com")
	_, _, repoID := createRepository(t, admin, "CtxMapPerm", "ctxmap-perm", "desc")
	other := registerAndLogin(t, env, "ctxmap-other@example.com")

	resp, _ := other.do("GET", "/api/v1/repositories/"+repoID+"/settings/context-mappings", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin GET mappings: expected 403, got %d", resp.StatusCode)
	}

	body, _ := json.Marshal(map[string]string{"local_context": "x", "registry_context": "y"})
	resp, _ = other.do("PUT", "/api/v1/repositories/"+repoID+"/settings/context-mappings", body)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin PUT mapping: expected 403, got %d", resp.StatusCode)
	}

	resp, _ = other.do("DELETE", "/api/v1/repositories/"+repoID+"/settings/context-mappings/x", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin DELETE mapping: expected 403, got %d", resp.StatusCode)
	}

	policyBody, _ := json.Marshal(map[string]string{"policy": "skip"})
	resp, _ = other.do("PUT", "/api/v1/repositories/"+repoID+"/settings/unmapped-policy", policyBody)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin PUT policy: expected 403, got %d", resp.StatusCode)
	}
}

// TestContextMapping_InboundWorker_SkipPolicy verifies the
// pull_all_from_registry worker's inbound context rewrite at the
// unit level: with policy=skip and an unmapped registry context,
// the mapper returns ("", false) so the concept is skipped. This
// exercises NewInboundContextMapper + mapContext directly without
// booting a full River worker (which would require a stub registry
// returning decompositions + a real Qdrant). The full worker-level
// test is left for a follow-up; this guards the mapping logic.
func TestContextMapping_InboundWorker_SkipPolicy(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "ctxmap-inbound@example.com")
	_, _, repoID := createRepository(t, admin, "CtxMapInbound", "ctxmap-inbound", "desc")
	queries := store.New(env.DB)
	repoUUID := pgRepoID(t, repoID)

	// No mappings + default skip policy → unmapped registry context
	// is skipped.
	mapper, err := tasks.NewInboundContextMapper(context.Background(), queries, repoUUID)
	if err != nil {
		t.Fatalf("building mapper: %v", err)
	}
	if got, ok := mapper.MapContext("ForeignLabel", nil); ok || got != "" {
		t.Errorf("skip policy: mapContext(ForeignLabel) = (%q, %v), want (\"\", false)", got, ok)
	}

	// Add a mapping ForeignLabel → Politician (must add Politician
	// as a local context first so the upsert validates).
	cbody, _ := json.Marshal(map[string]string{"context": "Politician"})
	admin.do("POST", "/api/v1/repositories/"+repoID+"/settings/contexts", cbody)
	mapBody, _ := json.Marshal(map[string]string{"local_context": "Politician", "registry_context": "ForeignLabel"})
	admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/context-mappings", mapBody)

	// Rebuild mapper → ForeignLabel now maps to Politician.
	mapper, err = tasks.NewInboundContextMapper(context.Background(), queries, repoUUID)
	if err != nil {
		t.Fatalf("rebuilding mapper: %v", err)
	}
	got, ok := mapper.MapContext("ForeignLabel", nil)
	if !ok || got != "Politician" {
		t.Errorf("after mapping: mapContext(ForeignLabel) = (%q, %v), want (Politician, true)", got, ok)
	}

	// Set policy to catch_all with Politician as the catch-all.
	policyBody, _ := json.Marshal(map[string]interface{}{"policy": "catch_all", "catch_all_context": "Politician"})
	admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/unmapped-policy", policyBody)
	mapper, err = tasks.NewInboundContextMapper(context.Background(), queries, repoUUID)
	if err != nil {
		t.Fatalf("rebuilding mapper after catch_all: %v", err)
	}
	// An unmapped registry context now routes to Politician.
	got, ok = mapper.MapContext("AnotherForeign", nil)
	if !ok || got != "Politician" {
		t.Errorf("catch_all: mapContext(AnotherForeign) = (%q, %v), want (Politician, true)", got, ok)
	}

	// Set policy to auto_add → the autoAdd callback is invoked.
	autoBody, _ := json.Marshal(map[string]interface{}{"policy": "auto_add"})
	admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/unmapped-policy", autoBody)
	mapper, err = tasks.NewInboundContextMapper(context.Background(), queries, repoUUID)
	if err != nil {
		t.Fatalf("rebuilding mapper after auto_add: %v", err)
	}
	called := false
	got, ok = mapper.MapContext("BrandNewLabel", func(label string) {
		called = true
		if label != "BrandNewLabel" {
			t.Errorf("autoAdd callback label = %q, want BrandNewLabel", label)
		}
	})
	if !ok || got != "BrandNewLabel" {
		t.Errorf("auto_add: mapContext(BrandNewLabel) = (%q, %v), want (BrandNewLabel, true)", got, ok)
	}
	if !called {
		t.Errorf("auto_add: autoAdd callback was not invoked")
	}
}

// TestContextMapping_OutboundWorker verifies the outbound context
// mapper logic directly: a mapped local context → its registry
// target; an unmapped local context in the registry vocab → verbatim;
// an unmapped local context absent from the vocab → skipped; an
// empty registry vocab → validation off (push verbatim).
func TestContextMapping_OutboundWorker(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "ctxmap-outbound@example.com")
	_, _, repoID := createRepository(t, admin, "CtxMapOutbound", "ctxmap-outbound", "desc")
	queries := store.New(env.DB)
	repoUUID := pgRepoID(t, repoID)

	// Add a custom local context "Product" and map it to "Politician".
	cbody, _ := json.Marshal(map[string]string{"context": "Product"})
	admin.do("POST", "/api/v1/repositories/"+repoID+"/settings/contexts", cbody)
	mapBody, _ := json.Marshal(map[string]string{"local_context": "Product", "registry_context": "Politician"})
	admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/context-mappings", mapBody)

	// nil registry client → registryEmpty=true → validation off.
	mapper, err := tasks.NewOutboundContextMapper(context.Background(), queries, nil, repoUUID)
	if err != nil {
		t.Fatalf("building mapper: %v", err)
	}
	// "Product" is mapped → "Politician".
	got, ok := mapper.MapContext("Product")
	if !ok || got != "Politician" {
		t.Errorf("mapped: mapContext(Product) = (%q, %v), want (Politician, true)", got, ok)
	}
	// "Unmapped" is unmapped, registry empty → verbatim.
	got, ok = mapper.MapContext("Unmapped")
	if !ok || got != "Unmapped" {
		t.Errorf("unmapped + empty vocab: mapContext(Unmapped) = (%q, %v), want (Unmapped, true)", got, ok)
	}

	// Now build a mapper with a non-empty registry vocab that
	// contains "Person" but not "Product" (which is mapped) nor
	// "Unmapped". We can't easily construct a *registry.Client
	// pointing at a stub here without the newRemoteEnvWithRegistry
	// helper, so test the logic via a hand-built mapper struct.
	mapper2 := tasks.NewOutboundContextMapperForTest(
		map[string]string{"product": "Politician"},
		map[string]bool{"person": true, "politician": true},
		false,
	)
	// Mapped → target.
	got, ok = mapper2.MapContext("Product")
	if !ok || got != "Politician" {
		t.Errorf("mapper2 mapped: got (%q, %v), want (Politician, true)", got, ok)
	}
	// Unmapped but in vocab → verbatim.
	got, ok = mapper2.MapContext("Person")
	if !ok || got != "Person" {
		t.Errorf("mapper2 unmapped-in-vocab: got (%q, %v), want (Person, true)", got, ok)
	}
	// Unmapped and absent → skip.
	got, ok = mapper2.MapContext("Foreign")
	if ok || got != "" {
		t.Errorf("mapper2 unmapped-absent: got (%q, %v), want (\"\", false)", got, ok)
	}
}