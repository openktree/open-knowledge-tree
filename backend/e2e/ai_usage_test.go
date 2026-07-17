//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/rbac"
)

// insertAIUsageRow inserts one ai_usage row directly via SQL so the
// dashboard endpoints have data to aggregate without needing a
// live AI provider. The row is written against the test env's
// default pool (which carries the okt_system search_path).
func insertAIUsageRow(t *testing.T, env *testutil.TestEnv, model, provider, operation string, prompt, completion, total int, repoID, sourceID *string) {
	t.Helper()
	ctx := context.Background()
	_, err := env.DB.Exec(ctx, `
		INSERT INTO okt_system.ai_usage (model, provider, operation, prompt_tokens, completion_tokens, total_tokens, repository_id, source_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())`,
		model, provider, operation, prompt, completion, total, repoID, sourceID,
	)
	if err != nil {
		t.Fatalf("inserting ai_usage row: %v", err)
	}
}

// TestAIUsageSummary_SysAdmin verifies the summary endpoint
// aggregates the per (provider, model, operation) rollup and
// surfaces a total_tokens grand total. A sysadmin session is
// required; a regular authenticated user gets 403.
func TestAIUsageSummary_SysAdmin(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "usage-admin@example.com")

	repoID := "11111111-1111-1111-1111-111111111111"
	sourceID := "22222222-2222-2222-2222-222222222222"
	insertAIUsageRow(t, env, "gpt-4o-mini", "openrouter", "chat", 100, 50, 150, nil, nil)
	insertAIUsageRow(t, env, "gpt-4o-mini", "openrouter", "fact_extraction", 200, 30, 230, &repoID, &sourceID)
	insertAIUsageRow(t, env, "llama3.2", "ollama", "chat", 80, 20, 100, &repoID, nil)
	// A row for a known-priced model (google/gemma-4-31b-it) so we can
	// assert the estimated_cost matches the per-1M pricing formula
	// (input 0.12 / 1M, output 0.4 / 1M). 6900 in + 408 out => 0.00099.
	// The model must be present in the env's config for LookupModel to
	// price it (the default test config has empty AI models).
	env.Config.Providers.AI.Models = []config.AIModelConfig{
		{ID: "google/gemma-4-31b-it", Provider: "openrouter", InputCostPer1M: 0.12, OutputCostPer1M: 0.4},
	}
	insertAIUsageRow(t, env, "google/gemma-4-31b-it", "openrouter", "chat", 6900, 408, 7308, nil, nil)

	resp, raw := admin.do("GET", "/api/v1/ai/usage/summary", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}

	var out struct {
		Rows []struct {
			Provider              string  `json:"provider"`
			Model                 string  `json:"model"`
			Operation             string  `json:"operation"`
			RequestCount          int64   `json:"request_count"`
			TotalPromptTokens     int64   `json:"total_prompt_tokens"`
			TotalCompletionTokens int64   `json:"total_completion_tokens"`
			TotalTokens           int64   `json:"total_tokens"`
			EstimatedCost         float64 `json:"estimated_cost"`
		} `json:"rows"`
		TotalPromptTokens     int64   `json:"total_prompt_tokens"`
		TotalCompletionTokens int64   `json:"total_completion_tokens"`
		TotalTokens           int64   `json:"total_tokens"`
		TotalCost             float64 `json:"total_cost"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding summary: %v", err)
	}
	if len(out.Rows) != 4 {
		t.Fatalf("expected 4 summary rows (one per provider/model/operation), got %d", len(out.Rows))
	}
	if out.TotalTokens != 150+230+100+7308 {
		t.Fatalf("expected total_tokens 7788, got %d", out.TotalTokens)
	}
	if out.TotalPromptTokens != 100+200+80+6900 {
		t.Fatalf("expected total_prompt_tokens 7280, got %d", out.TotalPromptTokens)
	}
	if out.TotalCompletionTokens != 50+30+20+408 {
		t.Fatalf("expected total_completion_tokens 508, got %d", out.TotalCompletionTokens)
	}
	// gemma row cost: 6900/1e6*0.12 + 408/1e6*0.4 = 0.000828 + 0.0001632
	var gemmaCost float64
	for _, row := range out.Rows {
		if row.Model == "google/gemma-4-31b-it" {
			gemmaCost = row.EstimatedCost
		}
	}
	wantGemmaCost := float64(6900)/1_000_000*0.12 + float64(408)/1_000_000*0.4
	if gemmaCost < wantGemmaCost-1e-9 || gemmaCost > wantGemmaCost+1e-9 {
		t.Fatalf("gemma estimated_cost: want %.9f, got %.9f", wantGemmaCost, gemmaCost)
	}
	if out.TotalCost < gemmaCost {
		t.Fatalf("total_cost (%.9f) should be >= gemma row cost (%.9f)", out.TotalCost, gemmaCost)
	}
}

// TestAIUsageSummary_DenyNonAdmin verifies a non-sysadmin
// authenticated user is rejected by the ai_usage.read permission
// gate (403, not 200 — confirming the route is permission-gated).
func TestAIUsageSummary_DenyNonAdmin(t *testing.T) {
	env := testutil.NewTestEnv(t)
	regular := newAuthClient(env.BaseURL)
	regular.register("usage-regular@example.com", "passw0rd!", "Regular")
	regular.token = loginUser(regular, "usage-regular@example.com", "passw0rd!")

	resp, _ := regular.do("GET", "/api/v1/ai/usage/summary", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for non-admin, got %d", resp.StatusCode)
	}
}

// TestAIUsageSummary_NoAuth verifies the endpoint requires
// authentication (401 without a token).
func TestAIUsageSummary_NoAuth(t *testing.T) {
	env := testutil.NewTestEnv(t)
	noAuth := newAuthClient(env.BaseURL)
	resp, _ := noAuth.do("GET", "/api/v1/ai/usage/summary", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d", resp.StatusCode)
	}
}

// TestAIUsageByDay verifies the time-bucketed endpoint returns one
// row per (bucket, model) and that the default bucket is "day".
func TestAIUsageByDay(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "usage-day-admin@example.com")

	insertAIUsageRow(t, env, "gpt-4o-mini", "openrouter", "chat", 100, 50, 150, nil, nil)
	insertAIUsageRow(t, env, "llama3.2", "ollama", "chat", 80, 20, 100, nil, nil)

	resp, raw := admin.do("GET", "/api/v1/ai/usage/by-day", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Bucket string `json:"bucket"`
		Rows   []struct {
			Bucket      time.Time `json:"bucket"`
			Model       string    `json:"model"`
			TotalTokens int64     `json:"total_tokens"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding by-day: %v", err)
	}
	if out.Bucket != "day" {
		t.Fatalf("expected default bucket 'day', got %q", out.Bucket)
	}
	if len(out.Rows) != 2 {
		t.Fatalf("expected 2 day rows (one per model), got %d", len(out.Rows))
	}
}

// TestAIUsageByDay_InvalidBucket verifies an invalid bucket value
// is rejected with 400 (input validation path).
func TestAIUsageByDay_InvalidBucket(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "usage-bucket-admin@example.com")
	resp, _ := admin.do("GET", "/api/v1/ai/usage/by-day?bucket=century", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid bucket, got %d", resp.StatusCode)
	}
}

// TestAIUsageByRepository verifies the per-repo rollup returns
// rows keyed by repository_id (NULL for unattributed rows) and
// that the repository_id query param filters to that repo.
func TestAIUsageByRepository(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "usage-repo-admin@example.com")

	repoA := "33333333-3333-3333-3333-333333333333"
	repoB := "44444444-4444-4444-4444-444444444444"
	insertAIUsageRow(t, env, "gpt-4o-mini", "openrouter", "chat", 100, 50, 150, nil, nil)
	insertAIUsageRow(t, env, "gpt-4o-mini", "openrouter", "chat", 200, 60, 260, &repoA, nil)
	insertAIUsageRow(t, env, "llama3.2", "ollama", "embedding", 90, 0, 90, &repoB, nil)

	// No filter: all three rows (two repo-keyed, one NULL).
	resp, raw := admin.do("GET", "/api/v1/ai/usage/by-repository", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Rows []struct {
			RepositoryID *string `json:"repository_id"`
			Model        string  `json:"model"`
			TotalTokens  int64   `json:"total_tokens"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding by-repository: %v", err)
	}
	if len(out.Rows) != 3 {
		t.Fatalf("expected 3 rows (null, repoA, repoB), got %d", len(out.Rows))
	}

	// Filter to repoA: only the repoA row.
	resp, raw = admin.do("GET", "/api/v1/ai/usage/by-repository?repository_id="+repoA, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	out = struct {
		Rows []struct {
			RepositoryID *string `json:"repository_id"`
			Model        string  `json:"model"`
			TotalTokens  int64   `json:"total_tokens"`
		} `json:"rows"`
	}{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding by-repository (filtered): %v", err)
	}
	if len(out.Rows) != 1 {
		t.Fatalf("expected 1 row for repoA, got %d", len(out.Rows))
	}
	if out.Rows[0].RepositoryID == nil || *out.Rows[0].RepositoryID != repoA {
		t.Fatalf("expected repository_id %s, got %v", repoA, out.Rows[0].RepositoryID)
	}
}

// TestAIUsageByOperation verifies the per (operation, model) rollup.
func TestAIUsageByOperation(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "usage-op-admin@example.com")

	insertAIUsageRow(t, env, "gpt-4o-mini", "openrouter", "chat", 100, 50, 150, nil, nil)
	insertAIUsageRow(t, env, "text-embedding-3-small", "openrouter", "embedding", 300, 0, 300, nil, nil)
	insertAIUsageRow(t, env, "gpt-4o-mini", "openrouter", "fact_extraction", 200, 30, 230, nil, nil)

	resp, raw := admin.do("GET", "/api/v1/ai/usage/by-operation", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Rows []struct {
			Operation    string `json:"operation"`
			Model        string `json:"model"`
			TotalTokens  int64  `json:"total_tokens"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding by-operation: %v", err)
	}
	if len(out.Rows) != 3 {
		t.Fatalf("expected 3 operation rows, got %d", len(out.Rows))
	}
}

// TestAIUsageBySource verifies the per (source_id, repo_id, model)
// rollup returns rows for attributed calls and NULL source_id for
// unattributed ones.
func TestAIUsageBySource(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "usage-source-admin@example.com")

	repoID := "55555555-5555-5555-5555-555555555555"
	sourceID := "66666666-6666-6666-6666-666666666666"
	insertAIUsageRow(t, env, "gpt-4o-mini", "openrouter", "fact_extraction", 200, 30, 230, &repoID, &sourceID)
	insertAIUsageRow(t, env, "gpt-4o-mini", "openrouter", "chat", 100, 50, 150, nil, nil)

	resp, raw := admin.do("GET", "/api/v1/ai/usage/by-source", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Rows []struct {
			SourceID     *string `json:"source_id"`
			RepositoryID *string `json:"repository_id"`
			Model        string  `json:"model"`
			TotalTokens  int64   `json:"total_tokens"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding by-source: %v", err)
	}
	if len(out.Rows) != 2 {
		t.Fatalf("expected 2 source rows, got %d", len(out.Rows))
	}
}

// TestAIUsageSummary_InvalidFrom verifies a malformed 'from'
// timestamp is rejected with 400 (filter validation path).
func TestAIUsageSummary_InvalidFrom(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "usage-invalidfrom-admin@example.com")
	resp, _ := admin.do("GET", "/api/v1/ai/usage/summary?from=not-a-timestamp", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid from, got %d", resp.StatusCode)
	}
}

// TestAIUsagePermission_IsValidObject verifies the ai_usage object
// is registered in the rbac validator so the admin role-assign
// endpoint can grant it to other roles (the "sysadmin only for
// now" decision leaves room for future per-role grants).
func TestAIUsagePermission_IsValidObject(t *testing.T) {
	if !rbac.IsValidObject(rbac.Objects.AIUsage) {
		t.Fatalf("expected ai_usage to be a valid rbac object")
	}
	if !rbac.IsValidAction(rbac.Actions.Read) {
		t.Fatalf("expected read to be a valid rbac action")
	}
}