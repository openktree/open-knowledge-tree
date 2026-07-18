//go:build e2e

package e2e_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/promptset"
)

// TestPromptsets_ListIncludesBuiltin verifies GET /api/v1/promptsets
// returns the built-in promptset (with its computed hash) plus any
// custom promptsets the caller owns. A fresh user sees only the
// built-in.
func TestPromptsets_ListIncludesBuiltin(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	client := registerTestUser(t, env, "ps-list@example.com", "passw0rd!", "PS List")

	resp, raw := client.do("GET", "/api/v1/promptsets", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /promptsets: status %d, body %s", resp.StatusCode, string(raw))
	}
	var list []promptset.Promptset
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode promptsets: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("expected at least the built-in promptset, got empty list")
	}
	if list[0].Hash != promptset.DefaultHash {
		t.Errorf("expected built-in hash %q first, got %q", promptset.DefaultHash, list[0].Hash)
	}
	if list[0].Source != promptset.BuiltinSource {
		t.Errorf("expected source %q, got %q", promptset.BuiltinSource, list[0].Source)
	}
	if !list[0].IsComplete() {
		t.Errorf("built-in promptset is incomplete: missing %v", list[0].MissingPhases())
	}
}

// TestPromptsets_CreateAndGet verifies POST /api/v1/promptsets creates
// a custom promptset with a server-computed hash, and GET
// /promptsets/{hash} returns it. The hash is the sha256 of the
// canonical JSON of the 8 phase strings, recomputed client-side here
// to confirm identity.
func TestPromptsets_CreateAndGet(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	client := registerTestUser(t, env, "ps-create@example.com", "passw0rd!", "PS Create")

	body, _ := json.Marshal(map[string]string{
		"name":                  "My Custom Philosophy",
		"fact_extraction":       "fact prompt v2",
		"image_fact_extraction": "image prompt v2",
		"concept_extraction":    "concept prompt v2",
		"refinement":            "refine prompt v2",
		"synthesis":             "synth prompt v2",
		"image_picker":          "picker prompt v2",
		"summarization":        "summary prompt v2",
		"posture":              "posture prompt v2",
	})
	resp, raw := client.do("POST", "/api/v1/promptsets", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /promptsets: status %d, body %s", resp.StatusCode, string(raw))
	}
	var created promptset.Promptset
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	// Recompute the expected hash client-side and confirm identity.
	expected := hashPhases(t, created)
	if created.Hash != expected {
		t.Errorf("server hash %q != expected %q", created.Hash, expected)
	}
	if created.Source != promptset.CustomSource {
		t.Errorf("expected source %q, got %q", promptset.CustomSource, created.Source)
	}

	// GET the created promptset back.
	resp, raw = client.do("GET", "/api/v1/promptsets/"+created.Hash, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /promptsets/%s: status %d, body %s", created.Hash, resp.StatusCode, string(raw))
	}
	var got promptset.Promptset
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode got: %v", err)
	}
	if got.Hash != created.Hash {
		t.Errorf("GET hash mismatch: %q != %q", got.Hash, created.Hash)
	}
}

// TestPromptsets_CreateRejectsIncomplete verifies the create
// endpoint rejects a promptset missing a phase with a 400 naming
// the missing phases.
func TestPromptsets_CreateRejectsIncomplete(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	client := registerTestUser(t, env, "ps-incomplete@example.com", "passw0rd!", "PS Incomplete")

	body, _ := json.Marshal(map[string]string{
		"name":                  "Incomplete",
		"fact_extraction":       "x",
		"image_fact_extraction": "", // missing
		"concept_extraction":    "x",
		"refinement":           "x",
		"synthesis":            "x",
		"image_picker":         "x",
		"summarization":        "x",
		"posture":              "",
	})
	resp, raw := client.do("POST", "/api/v1/promptsets", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for incomplete promptset, got %d: %s", resp.StatusCode, string(raw))
	}
}

// TestPromptsets_DeleteBuiltinRejected verifies the built-in
// promptset cannot be deleted.
func TestPromptsets_DeleteBuiltinRejected(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	client := registerTestUser(t, env, "ps-delbuiltin@example.com", "passw0rd!", "PS DelBuiltin")

	resp, _ := client.do("DELETE", "/api/v1/promptsets/"+promptset.DefaultHash, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 deleting built-in, got %d", resp.StatusCode)
	}
}

// TestPromptsets_DeleteOwn verifies a user can delete their own
// custom promptset and that it disappears from GET.
func TestPromptsets_DeleteOwn(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	client := registerTestUser(t, env, "ps-delown@example.com", "passw0rd!", "PS DelOwn")

	body, _ := json.Marshal(map[string]string{
		"name":                  "To Delete",
		"fact_extraction":       "f",
		"image_fact_extraction": "i",
		"concept_extraction":    "c",
		"refinement":           "r",
		"synthesis":           "s",
		"image_picker":         "p",
		"summarization":        "sum",
		"posture":             "pos",
	})
	resp, raw := client.do("POST", "/api/v1/promptsets", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST: status %d, body %s", resp.StatusCode, string(raw))
	}
	var created promptset.Promptset
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	resp, _ = client.do("DELETE", "/api/v1/promptsets/"+created.Hash, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("expected 204 deleting own promptset, got %d", resp.StatusCode)
	}
	resp, _ = client.do("GET", "/api/v1/promptsets/"+created.Hash, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 after delete, got %d", resp.StatusCode)
	}
}

// TestRepositoryPromptset_GetAndSet verifies the per-repo promptset
// GET/PUT endpoints round-trip the active + accepted hashes and
// reject unknown hashes.
func TestRepositoryPromptset_GetAndSet(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "ps-repo@example.com")
	_, _, repoID := createRepository(t, admin, "PSRepo", "ps-repo", "desc")

	// GET defaults: active_hash null, accepted_hashes empty, effective
	// = built-in.
	resp, raw := admin.do("GET", "/api/v1/repositories/"+repoID+"/settings/promptset", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET promptset: status %d, body %s", resp.StatusCode, string(raw))
	}
	var got struct {
		ActiveHash     *string  `json:"active_hash"`
		AcceptedHashes []string `json:"accepted_hashes"`
		EffectiveHash  string   `json:"effective_hash"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ActiveHash != nil {
		t.Errorf("expected nil active_hash, got %v", *got.ActiveHash)
	}
	if got.EffectiveHash != promptset.DefaultHash {
		t.Errorf("expected effective_hash %q, got %q", promptset.DefaultHash, got.EffectiveHash)
	}

	// Create a custom promptset to assign.
	psBody, _ := json.Marshal(map[string]string{
		"name":                  "Repo Philosophy",
		"fact_extraction":       "rf",
		"image_fact_extraction": "ri",
		"concept_extraction":    "rc",
		"refinement":           "rr",
		"synthesis":           "rs",
		"image_picker":         "rp",
		"summarization":        "rsum",
		"posture":             "rpos",
	})
	resp, raw = admin.do("POST", "/api/v1/promptsets", psBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST promptset: status %d, body %s", resp.StatusCode, string(raw))
	}
	var created promptset.Promptset
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// PUT assign it as active + accepted.
	setBody, _ := json.Marshal(map[string]interface{}{
		"active_hash":     created.Hash,
		"accepted_hashes": []string{created.Hash},
	})
	resp, _ = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/promptset", setBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT promptset: status %d", resp.StatusCode)
	}

	// GET reflects the assignment.
	resp, raw = admin.do("GET", "/api/v1/repositories/"+repoID+"/settings/promptset", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET after PUT: status %d", resp.StatusCode)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode after PUT: %v", err)
	}
	if got.ActiveHash == nil || *got.ActiveHash != created.Hash {
		t.Errorf("expected active_hash %q, got %v", created.Hash, got.ActiveHash)
	}
	if got.EffectiveHash != created.Hash {
		t.Errorf("expected effective_hash %q, got %q", created.Hash, got.EffectiveHash)
	}

	// PUT with an unknown hash is rejected.
	badBody, _ := json.Marshal(map[string]interface{}{
		"active_hash": "definitely-not-a-real-hash",
	})
	resp, _ = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/promptset", badBody)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for unknown active_hash, got %d", resp.StatusCode)
	}

	// PUT with null active_hash clears the override.
	clearBody, _ := json.Marshal(map[string]interface{}{
		"active_hash":     nil,
		"accepted_hashes": []string{},
	})
	resp, _ = admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/promptset", clearBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT clear: status %d", resp.StatusCode)
	}
	resp, raw = admin.do("GET", "/api/v1/repositories/"+repoID+"/settings/promptset", nil)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode after clear: %v", err)
	}
	if got.ActiveHash != nil {
		t.Errorf("expected nil active_hash after clear, got %v", *got.ActiveHash)
	}
}

// hashPhases recomputes the canonical promptset hash client-side
// (mirrors promptset.HashPromptset) so the e2e test can confirm the
// server computed the same identity. The input JSON must match the
// server's hashInput struct field-for-field in declaration order.
func hashPhases(t *testing.T, ps promptset.Promptset) string {
	t.Helper()
	in := struct {
		FactExtraction      string `json:"fact_extraction"`
		ImageFactExtraction string `json:"image_fact_extraction"`
		ConceptExtraction   string `json:"concept_extraction"`
		Refinement          string `json:"refinement"`
		Synthesis           string `json:"synthesis"`
		ImagePicker         string `json:"image_picker"`
		Summarization       string `json:"summarization"`
		Posture             string `json:"posture"`
	}{
		FactExtraction:      ps.FactExtraction,
		ImageFactExtraction: ps.ImageFactExtraction,
		ConceptExtraction:   ps.ConceptExtraction,
		Refinement:          ps.Refinement,
		Synthesis:           ps.Synthesis,
		ImagePicker:         ps.ImagePicker,
		Summarization:       ps.Summarization,
		Posture:             ps.Posture,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal hash input: %v", err)
	}
	sum := sha256.Sum256(bytes.TrimSpace(b))
	return hex.EncodeToString(sum[:])
}