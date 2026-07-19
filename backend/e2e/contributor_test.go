//go:build e2e

package e2e_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
)

// TestContributor_Default verifies a fresh repo defaults to
// anonymous attribution (contributor_anonymous=true,
// display_name=null) — the migration's back-fill — and that the
// GetSettings + GetContributor endpoints agree on that state.
func TestContributor_Default(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "contrib-default@example.com")
	_, _, repoID := createRepository(t, admin, "ContribDefault", "contrib-default", "desc")

	// GET /settings/contributor returns the defaults.
	resp, raw := admin.do("GET", "/api/v1/repositories/"+repoID+"/settings/contributor", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET contributor: status %d, body %s", resp.StatusCode, string(raw))
	}
	var got struct {
		DisplayName *string `json:"display_name"`
		Anonymous   bool    `json:"anonymous"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Anonymous {
		t.Errorf("expected anonymous=true by default, got false")
	}
	if got.DisplayName != nil {
		t.Errorf("expected display_name=null by default, got %q", *got.DisplayName)
	}

	// GET /settings also surfaces the same values so the page load
	// doesn't need a second round-trip.
	_, raw = admin.do("GET", "/api/v1/repositories/"+repoID+"/settings", nil)
	var s struct {
		ContributorDisplayName *string `json:"contributor_display_name"`
		ContributorAnonymous   bool    `json:"contributor_anonymous"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if !s.ContributorAnonymous {
		t.Errorf("expected settings.contributor_anonymous=true, got false")
	}
	if s.ContributorDisplayName != nil {
		t.Errorf("expected settings.contributor_display_name=null, got %q", *s.ContributorDisplayName)
	}
}

// TestContributor_SetDisplayName verifies the happy path: an admin
// opts out of anonymity and sets a display name; the next
// GET /settings/contributor reflects it.
func TestContributor_SetDisplayName(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "contrib-set@example.com")
	_, _, repoID := createRepository(t, admin, "ContribSet", "contrib-set", "desc")

	body, _ := json.Marshal(map[string]interface{}{
		"anonymous":    false,
		"display_name": "Alice's Research Lab",
	})
	resp, raw := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/contributor", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT contributor: status %d, body %s", resp.StatusCode, string(raw))
	}
	var got struct {
		DisplayName string  `json:"display_name"`
		Anonymous   bool    `json:"anonymous"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode put response: %v", err)
	}
	if got.Anonymous {
		t.Errorf("expected anonymous=false after PUT, got true")
	}
	if got.DisplayName != "Alice's Research Lab" {
		t.Errorf("expected display_name %q, got %q", "Alice's Research Lab", got.DisplayName)
	}

	// GET confirms the value persisted.
	resp, raw = admin.do("GET", "/api/v1/repositories/"+repoID+"/settings/contributor", nil)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if got.Anonymous || got.DisplayName != "Alice's Research Lab" {
		t.Errorf("round-trip mismatch: anonymous=%v display_name=%q", got.Anonymous, got.DisplayName)
	}
}

// TestContributor_AnonymousClearsName verifies that switching back
// to anonymous clears the stored display_name (so the column stays
// clean and the contribute worker sends the canonical anonymous
// marker, not a stale name the admin previously set).
func TestContributor_AnonymousClearsName(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "contrib-flip@example.com")
	_, _, repoID := createRepository(t, admin, "ContribFlip", "contrib-flip", "desc")

	// Set a name first.
	body, _ := json.Marshal(map[string]interface{}{
		"anonymous":    false,
		"display_name": "Bob's Lab",
	})
	if resp, _ := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/contributor", body); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT name: status %d", resp.StatusCode)
	}

	// Switch back to anonymous.
	anonBody, _ := json.Marshal(map[string]interface{}{
		"anonymous":    true,
		"display_name": nil,
	})
	resp, raw := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/contributor", anonBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT anonymous: status %d, body %s", resp.StatusCode, string(raw))
	}
	var got struct {
		DisplayName *string `json:"display_name"`
		Anonymous   bool    `json:"anonymous"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Anonymous {
		t.Errorf("expected anonymous=true, got false")
	}
	if got.DisplayName != nil {
		t.Errorf("expected display_name=null after switching to anonymous, got %q", *got.DisplayName)
	}
}

// TestContributor_Validation verifies the four validation rules:
//  1. anonymous=true with a non-empty display_name → 400 (server
//     clears the name, but the client must not send conflicting
//     state — actually the server tolerates this and clears; the
//     400 case below is "false without a name").
//  2. anonymous=false with an empty display_name → 400.
//  3. anonymous=false with a >120-char display_name → 400.
//  4. Empty body (no fields) → 400.
func TestContributor_Validation(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "contrib-val@example.com")
	_, _, repoID := createRepository(t, admin, "ContribVal", "contrib-val", "desc")

	// anonymous=false with empty display_name → 400.
	badEmpty, _ := json.Marshal(map[string]interface{}{
		"anonymous":    false,
		"display_name": "",
	})
	resp, _ := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/contributor", badEmpty)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for anonymous=false + empty name, got %d", resp.StatusCode)
	}

	// anonymous=false with null display_name → 400.
	badNull, _ := json.Marshal(map[string]interface{}{
		"anonymous":    false,
		"display_name": nil,
	})
	resp, _ = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/contributor", badNull)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for anonymous=false + null name, got %d", resp.StatusCode)
	}

	// anonymous=false with >120-char display_name → 400.
	longName := make([]byte, 121)
	for i := range longName {
		longName[i] = 'x'
	}
	badLong, _ := json.Marshal(map[string]interface{}{
		"anonymous":    false,
		"display_name": string(longName),
	})
	resp, _ = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/contributor", badLong)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for >120-char name, got %d", resp.StatusCode)
	}

	// Empty body (no fields) → 400.
	emptyBody, _ := json.Marshal(map[string]interface{}{})
	resp, _ = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/contributor", emptyBody)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for empty body, got %d", resp.StatusCode)
	}
}

// TestContributor_OmitOneField verifies that omitting one field
// keeps the other at its current value (the "partial update"
// contract). Set a name + anonymous=false, then PUT only
// anonymous=true and confirm the display_name was cleared by the
// server (because anonymous=true implies name=null). Then PUT only
// display_name="New Name" and confirm anonymous flips back to
// false (because anonymous=false is required when a name is set).
//
// Note: the server's combination rule is "anonymous=true ⇒ name
// must be null; anonymous=false ⇒ name must be non-empty". So
// setting only display_name="New Name" while the stored anonymous
// is true would conflict; the server rejects it. We instead verify
// the simpler omit-one path: PUT only display_name on a row that's
// already anonymous=false.
func TestContributor_OmitOneField(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "contrib-omit@example.com")
	_, _, repoID := createRepository(t, admin, "ContribOmit", "contrib-omit", "desc")

	// Initial: anonymous=false, name="Original".
	first, _ := json.Marshal(map[string]interface{}{
		"anonymous":    false,
		"display_name": "Original",
	})
	if resp, _ := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/contributor", first); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT initial: status %d", resp.StatusCode)
	}

	// PUT only display_name="Updated"; anonymous stays false.
	update, _ := json.Marshal(map[string]interface{}{
		"display_name": "Updated",
	})
	resp, raw := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/contributor", update)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT name-only: status %d, body %s", resp.StatusCode, string(raw))
	}
	var got struct {
		DisplayName string `json:"display_name"`
		Anonymous   bool   `json:"anonymous"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Anonymous {
		t.Errorf("expected anonymous to stay false when omitted, got true")
	}
	if got.DisplayName != "Updated" {
		t.Errorf("expected display_name %q, got %q", "Updated", got.DisplayName)
	}
}

// TestContributor_PermissionDeny verifies a non-admin (no
// repository:manage permission on this repo) cannot read or write
// the contributor identity.
func TestContributor_PermissionDeny(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "contrib-perm-admin@example.com")
	_, _, repoID := createRepository(t, admin, "ContribPerm", "contrib-perm", "desc")

	// A fresh non-admin user (registered but not the repo owner
	// and without repository:manage) gets 403 on both endpoints.
	other := registerAndLogin(t, env, "contrib-perm-user@example.com")
	resp, _ := other.do("GET", "/api/v1/repositories/"+repoID+"/settings/contributor", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for non-admin GET, got %d", resp.StatusCode)
	}
	body, _ := json.Marshal(map[string]interface{}{"anonymous": false, "display_name": "x"})
	resp, _ = other.do("PUT", "/api/v1/repositories/"+repoID+"/settings/contributor", body)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for non-admin PUT, got %d", resp.StatusCode)
	}
}