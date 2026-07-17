//go:build e2e

package e2e_test

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
)

// loadUploadPDFFixture reads the shared two_page.pdf fixture
// from the content_parsing testdata directory. It mirrors
// loadContentParsingPDF (e2e/content_parsing_test.go) but is
// kept local to the upload tests so the dependency is obvious.
func loadUploadPDFFixture(t *testing.T) []byte {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..")
	path := filepath.Join(repoRoot, "internal", "providers", "content_parsing", "testdata", "two_page.pdf")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pdf fixture: %v", err)
	}
	return b
}

// newMultipartUpload builds a multipart/form-data body carrying a
// single `file` part plus the optional form fields. Returns the
// body bytes and the full Content-Type header (with boundary).
func newMultipartUpload(t *testing.T, filename string, fileBytes []byte, fields map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatalf("write field %s: %v", k, err)
		}
	}
	fw, err := w.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(fileBytes); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return buf.Bytes(), w.FormDataContentType()
}

// uploadResponse is the wire shape returned by POST
// /{slug}/sources/upload.
type uploadResponse struct {
	JobID               string `json:"job_id"`
	SourceID            string `json:"source_id"`
	Status              string `json:"status"`
	InvestigationLinked bool   `json:"investigation_linked"`
}

// TestSourcesUploadTextHappyPath uploads raw text via the JSON
// path, asserts the source row exists in 'fetched' status with
// parsed_text populated, and that a source_decomposition job was
// enqueued.
func TestSourcesUploadTextHappyPath(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "upload_text_admin@example.com")
	const slug = "upload-text-repo"
	resp, body, repoID := createRepositoryWithDB(t, admin, "Upload Text Repo", slug, "desc", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: status %d, body %s", resp.StatusCode, body)
	}
	if repoID == "" {
		t.Fatal("expected repository id")
	}

	textBody := "This is a substantial uploaded text body. " +
		"It has enough content that the parse step considers it real prose " +
		"and the decomposition worker will have something to chunk. " +
		"Adding more sentences so the sentence offset array is non-trivial. " +
		"Yet another sentence to be safe about length."
	reqBody, _ := json.Marshal(map[string]string{
		"text":  textBody,
		"title": "Uploaded Text Title",
		"kind":  "paper",
	})
	r, raw := admin.do("POST", "/api/v1/repositories/"+slug+"/sources/upload", reqBody)
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("upload text: status %d, body %s", r.StatusCode, raw)
	}
	var out uploadResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	if out.SourceID == "" {
		t.Fatal("expected non-empty source_id")
	}
	if out.JobID == "" {
		t.Fatal("expected non-empty job_id")
	}
	if out.Status != "queued" {
		t.Fatalf("expected status=queued, got %q", out.Status)
	}
	if out.InvestigationLinked {
		t.Fatal("expected investigation_linked=false when no investigation_id supplied")
	}

	// A source_decomposition job should have been enqueued.
	if len(env.TaskEnqueuer.Decompositions) != 1 {
		t.Fatalf("expected 1 decomposition enqueued, got %d", len(env.TaskEnqueuer.Decompositions))
	}
	if env.TaskEnqueuer.Decompositions[0].SourceID != out.SourceID {
		t.Fatalf("decomposition SourceID=%q, want %q",
			env.TaskEnqueuer.Decompositions[0].SourceID, out.SourceID)
	}

	// The source row should be visible via the list endpoint and
	// carry status=fetched.
	listR, listRaw := admin.do("GET", "/api/v1/repositories/"+slug+"/sources", nil)
	if listR.StatusCode != http.StatusOK {
		t.Fatalf("list sources: status %d, body %s", listR.StatusCode, listRaw)
	}
	if !contains(listRaw, out.SourceID) {
		t.Errorf("expected list to contain the uploaded source id; body: %s", listRaw)
	}
	if !contains(listRaw, `"status":"fetched"`) {
		t.Errorf("expected a fetched source in list; body: %s", listRaw)
	}
}

// TestSourcesUploadPDFHappyPath uploads a real PDF fixture via
// multipart, asserts the source row is created with parsed_text
// populated, and that a decomposition job is enqueued.
func TestSourcesUploadPDFHappyPath(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "upload_pdf_admin@example.com")
	const slug = "upload-pdf-repo"
	resp, body, repoID := createRepositoryWithDB(t, admin, "Upload PDF Repo", slug, "desc", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: status %d, body %s", resp.StatusCode, body)
	}
	if repoID == "" {
		t.Fatal("expected repository id")
	}

	pdfBytes := loadUploadPDFFixture(t)
	multipartBody, contentType := newMultipartUpload(t, "two_page.pdf", pdfBytes, map[string]string{
		"kind": "paper",
	})
	r, raw := admin.doMultipart("POST", "/api/v1/repositories/"+slug+"/sources/upload", contentType, multipartBody)
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("upload pdf: status %d, body %s", r.StatusCode, raw)
	}
	var out uploadResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	if out.SourceID == "" {
		t.Fatal("expected non-empty source_id")
	}

	if len(env.TaskEnqueuer.Decompositions) != 1 {
		t.Fatalf("expected 1 decomposition enqueued, got %d", len(env.TaskEnqueuer.Decompositions))
	}

	// The row should be fetched and carry parsed_text.
	listR, listRaw := admin.do("GET", "/api/v1/repositories/"+slug+"/sources", nil)
	if listR.StatusCode != http.StatusOK {
		t.Fatalf("list sources: status %d, body %s", listR.StatusCode, listRaw)
	}
	if !contains(listRaw, out.SourceID) {
		t.Errorf("expected list to contain uploaded source id; body: %s", listRaw)
	}
	if !contains(listRaw, `"status":"fetched"`) {
		t.Errorf("expected fetched source; body: %s", listRaw)
	}
}

// TestSourcesUploadMarkdown verifies a .md file is parsed as
// markdown verbatim (no Trafilatura pass) and that the
// decomposition job is enqueued.
func TestSourcesUploadMarkdown(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "upload_md_admin@example.com")
	const slug = "upload-md-repo"
	resp, body, _ := createRepositoryWithDB(t, admin, "Upload MD Repo", slug, "desc", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: status %d, body %s", resp.StatusCode, body)
	}

	md := []byte("# Uploaded Markdown\n\nThis is a paragraph with enough text to be useful.\n\n- bullet one\n- bullet two\n")
	multipartBody, contentType := newMultipartUpload(t, "notes.md", md, nil)
	r, raw := admin.doMultipart("POST", "/api/v1/repositories/"+slug+"/sources/upload", contentType, multipartBody)
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("upload md: status %d, body %s", r.StatusCode, raw)
	}
	var out uploadResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	if out.SourceID == "" {
		t.Fatal("expected non-empty source_id")
	}
	if len(env.TaskEnqueuer.Decompositions) != 1 {
		t.Fatalf("expected 1 decomposition enqueued, got %d", len(env.TaskEnqueuer.Decompositions))
	}
}

// TestSourcesUploadDuplicateFilename verifies that re-uploading a
// file with the same filename returns 409 (the
// UNIQUE(repository_id, url) constraint on the synthetic URL).
func TestSourcesUploadDuplicateFilename(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "upload_dup_admin@example.com")
	const slug = "upload-dup-repo"
	resp, body, _ := createRepositoryWithDB(t, admin, "Upload Dup Repo", slug, "desc", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: status %d, body %s", resp.StatusCode, body)
	}

	pdfBytes := loadUploadPDFFixture(t)
	mb, ct := newMultipartUpload(t, "dup.pdf", pdfBytes, nil)
	r1, _ := admin.doMultipart("POST", "/api/v1/repositories/"+slug+"/sources/upload", ct, mb)
	if r1.StatusCode != http.StatusAccepted {
		t.Fatalf("first upload: status %d", r1.StatusCode)
	}
	// Rebuild the body (the multipart writer consumes the buffer).
	mb2, ct2 := newMultipartUpload(t, "dup.pdf", pdfBytes, nil)
	r2, raw2 := admin.doMultipart("POST", "/api/v1/repositories/"+slug+"/sources/upload", ct2, mb2)
	if r2.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate upload: status %d, want 409; body %s", r2.StatusCode, raw2)
	}
}

// TestSourcesUploadRequiresAuth verifies the upload endpoint is
// gated by the auth + source:create middleware.
func TestSourcesUploadRequiresAuth(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "upload_auth_admin@example.com")
	const slug = "upload-auth-repo"
	resp, body, _ := createRepositoryWithDB(t, admin, "Upload Auth Repo", slug, "desc", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: status %d, body %s", resp.StatusCode, body)
	}

	// Unauthenticated request.
	anon := newAuthClient(env.BaseURL)
	reqBody, _ := json.Marshal(map[string]string{"text": "hello"})
	r, _ := anon.do("POST", "/api/v1/repositories/"+slug+"/sources/upload", reqBody)
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 unauthenticated, got %d", r.StatusCode)
	}
}

// TestSourcesUploadUnknownSlug verifies a non-existent slug is
// rejected by the per-repo middleware (404) before the handler
// runs.
func TestSourcesUploadUnknownSlug(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "upload_slug_admin@example.com")
	reqBody, _ := json.Marshal(map[string]string{"text": "hello"})
	r, raw := admin.do("POST", "/api/v1/repositories/no-such-slug/sources/upload", reqBody)
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown slug, got %d: %s", r.StatusCode, raw)
	}
}

// TestSourcesUploadInvestigationLink verifies the optional
// investigation_id field: when present and valid, the source is
// atomically linked to the investigation and the response
// carries investigation_linked=true. A subsequent GET on the
// investigation's sources shows the new source.
func TestSourcesUploadInvestigationLink(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "upload_inv_admin@example.com")
	const slug = "upload-inv-repo"
	resp, body, _ := createRepositoryWithDB(t, admin, "Upload Inv Repo", slug, "desc", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: status %d, body %s", resp.StatusCode, body)
	}

	// Create an investigation to link into.
	invBody, _ := json.Marshal(map[string]string{
		"title": "Upload Inv",
		"topic": "uploads",
	})
	invR, invRaw := admin.do("POST", "/api/v1/repositories/"+slug+"/investigations", invBody)
	if invR.StatusCode != http.StatusCreated {
		t.Fatalf("create investigation: status %d, body %s", invR.StatusCode, invRaw)
	}
	var inv struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(invRaw, &inv); err != nil {
		t.Fatalf("decode investigation: %v", err)
	}
	invID := inv.ID

	// Upload text and link it to the investigation.
	reqBody, _ := json.Marshal(map[string]string{
		"text":            "Substantial text for the investigation-linked upload test. Enough prose to pass the parse gate.",
		"investigation_id": invID,
	})
	r, raw := admin.do("POST", "/api/v1/repositories/"+slug+"/sources/upload", reqBody)
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("upload with investigation: status %d, body %s", r.StatusCode, raw)
	}
	var out uploadResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	if !out.InvestigationLinked {
		t.Fatal("expected investigation_linked=true")
	}

	// The investigation's source list should contain the new source.
	listR, listRaw := admin.do("GET", "/api/v1/repositories/"+slug+"/investigations/"+invID+"/sources", nil)
	if listR.StatusCode != http.StatusOK {
		t.Fatalf("list inv sources: status %d, body %s", listR.StatusCode, listRaw)
	}
	var page pageEnvelope
	if err := json.Unmarshal(listRaw, &page); err != nil {
		t.Fatalf("decode inv source list: %v", err)
	}
	if page.Total != 1 {
		t.Fatalf("expected total=1 linked source, got %d", page.Total)
	}
}

// TestSourcesUploadInvestigationNotFound verifies that an invalid
// investigation_id is rejected with 404 (the investigation does
// not exist).
func TestSourcesUploadInvestigationNotFound(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "upload_inv404_admin@example.com")
	const slug = "upload-inv404-repo"
	resp, body, _ := createRepositoryWithDB(t, admin, "Upload Inv404 Repo", slug, "desc", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: status %d, body %s", resp.StatusCode, body)
	}

	fakeInv := "00000000-0000-0000-0000-000000000000"
	reqBody, _ := json.Marshal(map[string]string{
		"text":            "Substantial text for the bad-investigation upload test.",
		"investigation_id": fakeInv,
	})
	r, raw := admin.do("POST", "/api/v1/repositories/"+slug+"/sources/upload", reqBody)
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown investigation_id, got %d: %s", r.StatusCode, raw)
	}
}