//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
)

// TestReportsCRUD covers the report lifecycle: create (JSON + upload),
// get (with annotations), list, update (with re-annotation enqueue),
// delete, and the annotate endpoint. Uses a sysadmin client so every
// permission is granted.
func TestReportsCRUD(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "report_admin@example.com")
	const slug = "report-repo"
	if resp, body, _ := createRepositoryWithDB(t, admin, "Report Repo", slug, "desc", ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", resp.StatusCode, body)
	}

	// 1. Create a report via JSON.
	createBody, _ := json.Marshal(map[string]string{
		"title": "Climate impacts report",
		"topic": "sea level rise",
		"text":  "# Climate impacts\n\nSea level is rising due to thermal expansion of the oceans.\nGlaciers are melting at an accelerating rate.",
	})
	createResp, createRaw := admin.do("POST", "/api/v1/repositories/"+slug+"/reports", createBody)
	if createResp.StatusCode != http.StatusAccepted {
		t.Fatalf("create report: status %d, body %s", createResp.StatusCode, createRaw)
	}
	var created struct {
		ReportID string `json:"report_id"`
		JobID    string `json:"job_id"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal(createRaw, &created); err != nil {
		t.Fatalf("decoding create response: %v", err)
	}
	if created.ReportID == "" || created.JobID == "" {
		t.Fatalf("expected report_id + job_id, got %+v", created)
	}
	reportID := created.ReportID

	// 1a. The enqueuer recorded the annotate_report call.
	annotates := env.TaskEnqueuer.ReportAnnotatesSnapshot()
	if len(annotates) != 1 {
		t.Fatalf("expected 1 annotate_report enqueue, got %d", len(annotates))
	}
	if annotates[0].ReportID != reportID {
		t.Fatalf("enqueue report_id mismatch: got %s", annotates[0].ReportID)
	}

	// 2. Get the report.
	getResp, getRaw := admin.do("GET", "/api/v1/repositories/"+slug+"/reports/"+reportID, nil)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("get report: status %d, body %s", getResp.StatusCode, getRaw)
	}
	var got struct {
		Report struct {
			Title string `json:"title"`
			Topic *string `json:"topic"`
			Status string `json:"status"`
		} `json:"report"`
		Annotations []json.RawMessage `json:"annotations"`
	}
	if err := json.Unmarshal(getRaw, &got); err != nil {
		t.Fatalf("decoding report: %v", err)
	}
	if got.Report.Title != "Climate impacts report" || got.Report.Topic == nil || *got.Report.Topic != "sea level rise" {
		t.Fatalf("unexpected report: %+v", got)
	}

	// 3. List reports — should contain the one we created.
	listResp, listRaw := admin.do("GET", "/api/v1/repositories/"+slug+"/reports", nil)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list reports: status %d, body %s", listResp.StatusCode, listRaw)
	}
	var list pageEnvelope
	if err := json.Unmarshal(listRaw, &list); err != nil {
		t.Fatalf("decoding list: %v", err)
	}
	if list.Total != 1 {
		t.Fatalf("expected total=1, got %d", list.Total)
	}

	// 4. Create a report via multipart upload (.md).
	var mpBuf bytes.Buffer
	mw := multipart.NewWriter(&mpBuf)
	_ = mw.WriteField("title", "Second report")
	fileWriter, _ := mw.CreateFormFile("file", "report.md")
	fileWriter.Write([]byte("# Second\n\nThis is a second report with enough text to be meaningful."))
	mw.Close()
	uploadResp, uploadRaw := admin.doMultipart("POST", "/api/v1/repositories/"+slug+"/reports/upload", mw.FormDataContentType(), mpBuf.Bytes())
	if uploadResp.StatusCode != http.StatusAccepted {
		t.Fatalf("upload report: status %d, body %s", uploadResp.StatusCode, uploadRaw)
	}
	var uploaded struct {
		ReportID string `json:"report_id"`
	}
	if err := json.Unmarshal(uploadRaw, &uploaded); err != nil {
		t.Fatalf("decoding upload response: %v", err)
	}
	if uploaded.ReportID == "" {
		t.Fatal("expected uploaded report_id")
	}

	// 5. Update the first report (new body -> re-annotation enqueue).
	updBody, _ := json.Marshal(map[string]string{
		"title": "Climate impacts v2",
		"topic": "",
		"text":  "# Climate impacts v2\n\nUpdated body text that should trigger re-annotation.",
	})
	updResp, updRaw := admin.do("PUT", "/api/v1/repositories/"+slug+"/reports/"+reportID, updBody)
	if updResp.StatusCode != http.StatusOK {
		t.Fatalf("update report: status %d, body %s", updResp.StatusCode, updRaw)
	}
	var updated struct {
		Title  string `json:"title"`
		BodyMd string `json:"body_md"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(updRaw, &updated); err != nil {
		t.Fatalf("decoding updated: %v", err)
	}
	if updated.Title != "Climate impacts v2" {
		t.Fatalf("unexpected updated title: %s", updated.Title)
	}
	if !strings.Contains(updated.BodyMd, "Climate impacts v2") {
		t.Fatalf("unexpected body_md: %s", updated.BodyMd)
	}

	// 5a. The update enqueued a second annotate_report (re-annotation).
	if got := env.TaskEnqueuer.ReportAnnotateCount(); got < 2 {
		t.Fatalf("expected re-annotation enqueue, got %d total annotate calls", got)
	}

	// 6. Manually re-annotate via the /annotate endpoint.
	annResp, annRaw := admin.do("POST", "/api/v1/repositories/"+slug+"/reports/"+reportID+"/annotate", nil)
	if annResp.StatusCode != http.StatusAccepted {
		t.Fatalf("annotate report: status %d, body %s", annResp.StatusCode, annRaw)
	}
	var annJob struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(annRaw, &annJob); err != nil {
		t.Fatalf("decoding annotate response: %v", err)
	}
	if annJob.JobID == "" {
		t.Fatal("expected annotate job_id")
	}

	// 7. List annotations (empty until the worker runs, but should 200).
	annListResp, _ := admin.do("GET", "/api/v1/repositories/"+slug+"/reports/"+reportID+"/annotations", nil)
	if annListResp.StatusCode != http.StatusOK {
		t.Fatalf("list annotations: status %d", annListResp.StatusCode)
	}

	// 8. Delete the report.
	delResp, delRaw := admin.do("DELETE", "/api/v1/repositories/"+slug+"/reports/"+reportID, nil)
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete report: status %d, body %s", delResp.StatusCode, delRaw)
	}

	// 8a. Get is now 404.
	get2Resp, _ := admin.do("GET", "/api/v1/repositories/"+slug+"/reports/"+reportID, nil)
	if get2Resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", get2Resp.StatusCode)
	}
}

// TestReportsCrossRepoIsolation verifies that a report in one
// repository is not visible from another repository's routes (a 404,
// not a leak).
func TestReportsCrossRepoIsolation(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "cross_repo_report@example.com")
	const slugA = "cross-report-a"
	const slugB = "cross-report-b"
	if resp, body, _ := createRepositoryWithDB(t, admin, "A", slugA, "", ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo A: %d %s", resp.StatusCode, body)
	}
	if resp, body, _ := createRepositoryWithDB(t, admin, "B", slugB, "", ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo B: %d %s", resp.StatusCode, body)
	}

	createBody, _ := json.Marshal(map[string]string{
		"title": "A only",
		"text":  "Report in A.",
	})
	createResp, createRaw := admin.do("POST", "/api/v1/repositories/"+slugA+"/reports", createBody)
	if createResp.StatusCode != http.StatusAccepted {
		t.Fatalf("create in A: %d %s", createResp.StatusCode, createRaw)
	}
	var created struct {
		ReportID string `json:"report_id"`
	}
	json.Unmarshal(createRaw, &created)

	// Query via repo B's slug → 404 (not a leak).
	resp, _ := admin.do("GET", "/api/v1/repositories/"+slugB+"/reports/"+created.ReportID, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-repo get: expected 404, got %d", resp.StatusCode)
	}

	// Repo B's list is empty.
	resp, body := admin.do("GET", "/api/v1/repositories/"+slugB+"/reports", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list in B: %d", resp.StatusCode)
	}
	var list pageEnvelope
	json.Unmarshal(body, &list)
	if list.Total != 0 {
		t.Fatalf("expected B list total=0, got %d", list.Total)
	}
}

// TestReportsNesting covers the parent_id / children_ids sub-report
// feature: create-with-parent, create-with-children (the meta-
// synthesis case), reparent via update, cycle rejection, cascade on
// parent delete, and the parent_id field in list/get responses.
func TestReportsNesting(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "nesting_report@example.com")
	const slug = "nesting-report-repo"
	if resp, body, _ := createRepositoryWithDB(t, admin, "Nesting Repo", slug, "", ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", resp.StatusCode, body)
	}

	create := func(t *testing.T, payload map[string]interface{}) string {
		t.Helper()
		b, _ := json.Marshal(payload)
		resp, raw := admin.do("POST", "/api/v1/repositories/"+slug+"/reports", b)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("create report: %d %s", resp.StatusCode, raw)
		}
		var out struct {
			ReportID string `json:"report_id"`
		}
		json.Unmarshal(raw, &out)
		if out.ReportID == "" {
			t.Fatal("expected report_id")
		}
		return out.ReportID
	}

	// 1. Create two top-level children first.
	childA := create(t, map[string]interface{}{"title": "Child A", "text": "alpha"})
	childB := create(t, map[string]interface{}{"title": "Child B", "text": "beta"})

	// 2. Create a parent and reparent both children under it via
	// children_ids (the meta-synthesis flow).
	parentID := create(t, map[string]interface{}{
		"title":        "Meta synthesis",
		"text":         "combines A and B",
		"children_ids": []string{childA, childB},
	})

	// 2a. Each child's parent_id is now the parent.
	for _, child := range []string{childA, childB} {
		resp, raw := admin.do("GET", "/api/v1/repositories/"+slug+"/reports/"+child, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("get child %s: %d", child, resp.StatusCode)
		}
		var got struct {
			Report struct {
				ParentID string `json:"parent_id"`
			} `json:"report"`
		}
		json.Unmarshal(raw, &got)
		if got.Report.ParentID != parentID {
			t.Fatalf("child %s parent_id = %q, want %q", child, got.Report.ParentID, parentID)
		}
	}

	// 2b. create-with-parent_id path: create a new child under parentID.
	childC := create(t, map[string]interface{}{
		"title":     "Child C",
		"text":      "gamma",
		"parent_id": parentID,
	})
	{
		resp, raw := admin.do("GET", "/api/v1/repositories/"+slug+"/reports/"+childC, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("get childC: %d", resp.StatusCode)
		}
		var got struct {
			Report struct {
				ParentID string `json:"parent_id"`
			} `json:"report"`
		}
		json.Unmarshal(raw, &got)
		if got.Report.ParentID != parentID {
			t.Fatalf("childC parent_id = %q, want %q", got.Report.ParentID, parentID)
		}
	}

	// 3. List reports — rows include parent_id.
	listResp, listRaw := admin.do("GET", "/api/v1/repositories/"+slug+"/reports", nil)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list: %d", listResp.StatusCode)
	}
	var list pageEnvelope
	json.Unmarshal(listRaw, &list)
	if list.Total != 4 {
		t.Fatalf("expected total=4, got %d", list.Total)
	}
	parentIDsByTitle := map[string]string{}
	for _, raw := range list.Data {
		var r struct {
			Title    string  `json:"title"`
			ParentID *string `json:"parent_id"`
		}
		json.Unmarshal(raw, &r)
		pid := ""
		if r.ParentID != nil {
			pid = *r.ParentID
		}
		parentIDsByTitle[r.Title] = pid
	}
	if parentIDsByTitle["Meta synthesis"] != "" {
		t.Fatalf("parent should be top-level, got parent_id %q", parentIDsByTitle["Meta synthesis"])
	}
	if parentIDsByTitle["Child A"] != parentID {
		t.Fatalf("Child A parent_id = %q, want %q", parentIDsByTitle["Child A"], parentID)
	}

	// 4. Reparent via update: move childC to top-level (clear parent).
	updBody, _ := json.Marshal(map[string]interface{}{
		"title":    "Child C",
		"text":     "gamma",
		"parent_id": "",
	})
	updResp, _ := admin.do("PUT", "/api/v1/repositories/"+slug+"/reports/"+childC, updBody)
	if updResp.StatusCode != http.StatusOK {
		t.Fatalf("reparent update: %d", updResp.StatusCode)
	}
	{
		resp, raw := admin.do("GET", "/api/v1/repositories/"+slug+"/reports/"+childC, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("get childC after reparent: %d", resp.StatusCode)
		}
		var got struct {
			Report struct {
				ParentID *string `json:"parent_id"`
			} `json:"report"`
		}
		json.Unmarshal(raw, &got)
		if got.Report.ParentID != nil && *got.Report.ParentID != "" {
			t.Fatalf("childC parent_id should be empty, got %q", *got.Report.ParentID)
		}
	}

	// 5. Reparent childC back under parentID via update.
	updBody2, _ := json.Marshal(map[string]interface{}{
		"title":     "Child C",
		"text":      "gamma",
		"parent_id": parentID,
	})
	updResp2, _ := admin.do("PUT", "/api/v1/repositories/"+slug+"/reports/"+childC, updBody2)
	if updResp2.StatusCode != http.StatusOK {
		t.Fatalf("reparent update back: %d", updResp2.StatusCode)
	}

	// 6. Cycle rejection: reparent parentID under childA (its own
	// descendant) → 400.
	cycleBody, _ := json.Marshal(map[string]interface{}{
		"title":     "Meta synthesis",
		"text":      "combines A and B",
		"parent_id": childA,
	})
	cycleResp, _ := admin.do("PUT", "/api/v1/repositories/"+slug+"/reports/"+parentID, cycleBody)
	if cycleResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("cycle reparent: expected 400, got %d", cycleResp.StatusCode)
	}

	// 7. Self-parent rejection on create → 400.
	selfBody, _ := json.Marshal(map[string]interface{}{
		"title":     "Self",
		"text":      "x",
		"parent_id": parentID,
	})
	// Reuse parentID as its own parent is not possible on create, so
	// test self-parent via update instead.
	selfUpd, _ := json.Marshal(map[string]interface{}{
		"title":     "Meta synthesis",
		"text":      "combines A and B",
		"parent_id": parentID,
	})
	selfResp, _ := admin.do("PUT", "/api/v1/repositories/"+slug+"/reports/"+parentID, selfUpd)
	if selfResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("self-parent update: expected 400, got %d", selfResp.StatusCode)
	}
	_ = selfBody

	// 8. Cross-repo children_ids: reference a report in another repo → 400.
	const slugOther = "nesting-other-repo"
	if resp, body, _ := createRepositoryWithDB(t, admin, "Other", slugOther, "", ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create other repo: %d %s", resp.StatusCode, body)
	}
	otherBody, _ := json.Marshal(map[string]string{"title": "Other", "text": "z"})
	otherResp, otherRaw := admin.do("POST", "/api/v1/repositories/"+slugOther+"/reports", otherBody)
	if otherResp.StatusCode != http.StatusAccepted {
		t.Fatalf("create other report: %d %s", otherResp.StatusCode, otherRaw)
	}
	var otherCreated struct {
		ReportID string `json:"report_id"`
	}
	json.Unmarshal(otherRaw, &otherCreated)
	crossBody, _ := json.Marshal(map[string]interface{}{
		"title":        "Cross parent",
		"text":         "y",
		"children_ids": []string{otherCreated.ReportID},
	})
	crossResp, _ := admin.do("POST", "/api/v1/repositories/"+slug+"/reports", crossBody)
	if crossResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("cross-repo children_ids: expected 400, got %d", crossResp.StatusCode)
	}

	// 9. Cascade: delete the parent → children gone too.
	delResp, _ := admin.do("DELETE", "/api/v1/repositories/"+slug+"/reports/"+parentID, nil)
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete parent: %d", delResp.StatusCode)
	}
	for _, child := range []string{childA, childB, childC} {
		resp, _ := admin.do("GET", "/api/v1/repositories/"+slug+"/reports/"+child, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("child %s should be gone after parent delete, got %d", child, resp.StatusCode)
		}
	}
}

// TestReportsPermissionDenied verifies that an unauthenticated user
// (no token) and an authenticated user without the report:create
// permission cannot create a report. The sysadmin can; a viewer can
// list (read) but not create.
func TestReportsPermissionDenied(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "perm_report_admin@example.com")
	const slug = "perm-report-repo"
	resp, body, repoID := createRepositoryWithDB(t, admin, "Perm Report Repo", slug, "", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", resp.StatusCode, body)
	}

	// Unauthenticated → 401.
	anon := newAuthClient(env.BaseURL)
	createBody, _ := json.Marshal(map[string]string{"title": "no auth", "text": "x"})
	anonResp, _ := anon.do("POST", "/api/v1/repositories/"+slug+"/reports", createBody)
	if anonResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth create: expected 401, got %d", anonResp.StatusCode)
	}

	// Authenticated viewer WITHOUT report:create: register a fresh
	// user grouped only into "viewer" (read-only on reports per seed).
	viewer := newAuthClient(env.BaseURL)
	if r, _ := viewer.register("viewer_report@example.com", "passw0rd!", "Viewer"); r.StatusCode != http.StatusCreated {
		t.Fatalf("viewer register: %d", r.StatusCode)
	}
	viewer.token = loginUser(viewer, "viewer_report@example.com", "passw0rd!")

	resp, meBody := viewer.do("GET", "/api/v1/users/me", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("viewer /me: %d", resp.StatusCode)
	}
	var me struct {
		ID string `json:"id"`
	}
	json.Unmarshal(meBody, &me)
	if _, err := env.DB.Exec(
		context.Background(),
		`DELETE FROM casbin_rule WHERE p_type='g' AND v0=$1 AND v1='user'`,
		me.ID,
	); err != nil {
		t.Fatalf("removing legacy user grouping: %v", err)
	}
	if _, err := env.DB.Exec(
		context.Background(),
		`INSERT INTO casbin_rule (p_type, v0, v1, v2) VALUES ('g', $1, 'viewer', $2)`,
		me.ID, repoID,
	); err != nil {
		t.Fatalf("seeding viewer grouping: %v", err)
	}
	if err := env.RBAC.LoadPolicy(); err != nil {
		t.Fatalf("reloading RBAC: %v", err)
	}

	// Viewer cannot create → 403.
	resp, _ = viewer.do("POST", "/api/v1/repositories/"+slug+"/reports", createBody)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer create: expected 403, got %d", resp.StatusCode)
	}

	// Viewer CAN list → 200.
	resp, listBody := viewer.do("GET", "/api/v1/repositories/"+slug+"/reports", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("viewer list: expected 200, got %d (body=%s)", resp.StatusCode, string(listBody))
	}
}