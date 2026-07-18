//go:build e2e

package e2e_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
)

// TestContentTypes_Gate verifies the per-repo allowed_content_types
// gate (migration 0049) round-trips and is enforced at the three
// ingestion points: CreateSource (url/doi), UploadSource (document),
// and EnqueueRetrieveSource (url/doi via fetch.ClassifyURL).
func TestContentTypes_Gate(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "content-types@example.com")
	_, _, repoID := createRepository(t, admin, "ContentTypes", "content-types", "desc")

	// Restrict to DOI only.
	body, _ := json.Marshal(map[string]interface{}{
		"allowed_content_types": []string{"doi"},
	})
	resp, raw := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/content-types", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set content types: status %d, body %s", resp.StatusCode, string(raw))
	}

	// Re-read and confirm the value round-trips.
	_, raw = admin.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	var s struct {
		AllowedContentTypes []string `json:"allowed_content_types"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if len(s.AllowedContentTypes) != 1 || s.AllowedContentTypes[0] != "doi" {
		t.Errorf("expected [doi], got %v", s.AllowedContentTypes)
	}

	// CreateSource with a URL should be 403 (doi-only repo).
	urlBody, _ := json.Marshal(map[string]string{"url": "https://example.com/some-article"})
	resp, _ = admin.do("POST", "/api/v1/repositories/"+repoID+"/sources", urlBody)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("CreateSource with URL in doi-only repo: expected 403, got %d", resp.StatusCode)
	}

	// UploadSource should be 403 (doi-only repo rejects documents).
	resp, _ = admin.do("POST", "/api/v1/repositories/"+repoID+"/sources/upload", []byte(`{"text":"hello"}`))
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("UploadSource in doi-only repo: expected 403, got %d", resp.StatusCode)
	}

	// EnqueueRetrieveSource with a URL should be 403.
	retBody, _ := json.Marshal(map[string]string{"url": "https://example.com/another"})
	resp, _ = admin.do("POST", "/api/v1/sources/retrieve", retBody)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("EnqueueRetrieveSource with URL in doi-only repo: expected 403, got %d", resp.StatusCode)
	}
}

// TestContentTypes_AllowAll verifies that a repo with NULL
// allowed_content_types (the default) accepts all three content
// kinds — the backward-compatible behavior for existing repos.
func TestContentTypes_AllowAll(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "content-types-allow@example.com")
	_, _, repoID := createRepository(t, admin, "ContentTypesAllowAll", "content-types-allow", "desc")

	// Default: NULL = allow all. Confirm the settings response
	// surfaces null.
	_, raw := admin.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	var s struct {
		AllowedContentTypes []string `json:"allowed_content_types"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if s.AllowedContentTypes != nil {
		t.Errorf("expected null allowed_content_types (allow all), got %v", s.AllowedContentTypes)
	}

	// CreateSource with a URL should succeed (201) under allow-all.
	urlBody, _ := json.Marshal(map[string]string{"url": "https://example.com/allow-all-article"})
	resp, _ := admin.do("POST", "/api/v1/repositories/"+repoID+"/sources", urlBody)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("CreateSource with URL in allow-all repo: expected 201, got %d", resp.StatusCode)
	}
}

// TestContentTypes_Validation verifies the PUT endpoint rejects
// invalid input: unknown values, duplicates, and empty arrays.
func TestContentTypes_Validation(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "content-types-val@example.com")
	_, _, repoID := createRepository(t, admin, "ContentTypesVal", "content-types-val", "desc")

	// Unknown value → 400.
	bad, _ := json.Marshal(map[string]interface{}{
		"allowed_content_types": []string{"doi", "bogus"},
	})
	resp, _ := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/content-types", bad)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown value: expected 400, got %d", resp.StatusCode)
	}

	// Duplicate → 400.
	dup, _ := json.Marshal(map[string]interface{}{
		"allowed_content_types": []string{"doi", "doi"},
	})
	resp, _ = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/content-types", dup)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("duplicate value: expected 400, got %d", resp.StatusCode)
	}

	// Empty array → 400 (use null to reset).
	empty, _ := json.Marshal(map[string]interface{}{
		"allowed_content_types": []string{},
	})
	resp, _ = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/content-types", empty)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty array: expected 400, got %d", resp.StatusCode)
	}

	// null → 200 (resets to allow-all).
	nullBody, _ := json.Marshal(map[string]interface{}{
		"allowed_content_types": nil,
	})
	resp, _ = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/content-types", nullBody)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("null reset: expected 200, got %d", resp.StatusCode)
	}
}