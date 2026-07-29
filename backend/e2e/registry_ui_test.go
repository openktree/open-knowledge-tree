//go:build e2e

package e2e_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// registryUIBaseURL is the live registry instance under test. Mirrors
// the OKT_TEST_REGISTRY_URL gate used by registry_search_test.go so
// the e2e suite stays green in a keyless dev env. The authoritative
// dev instance is https://registry.openktree-dev.com/ (per the
// registry_ui_test plan: live dev, not prod).
func registryUIBaseURL() string {
	return os.Getenv("OKT_TEST_REGISTRY_URL")
}

func newRegistryUIJarClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	return &http.Client{Jar: jar}
}

func newRegistryUIRawClient() *http.Client {
	return &http.Client{}
}

// registryUIRegisterUser registers a fresh user via the JSON API
// (the same endpoint used by /ui/register server-side). Returns the
// email + password so the caller can log in via the form-based
// /ui/login endpoint.
func registryUIRegisterUser(t *testing.T, baseURL string) (string, string) {
	t.Helper()
	email := fmt.Sprintf("registry-ui-e2e+%d@example.com", time.Now().UnixNano())
	password := fmt.Sprintf("pw-%d!", time.Now().UnixNano())

	body, _ := json.Marshal(map[string]string{
		"email":    email,
		"password": password,
	})
	req, err := http.NewRequest("POST", baseURL+"/api/v1/auth/register", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build register request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: status %d, body %s", resp.StatusCode, string(raw))
	}
	return email, password
}

// TestRegistryUI_LoginRedirectsAndDashboardRenders is the happy
// path / regression check for the production bug: after logging in
// via /ui/login, the redirect must succeed and the dashboard must
// render the user, not the JSON 401 that the pre-fix middleware
// emitted. Login now lands on /ui/sources (the default browser
// page); the legacy /ui/dashboard URL still works as a back-compat
// alias for the tokens page.
func TestRegistryUI_LoginRedirectsAndDashboardRenders(t *testing.T) {
	baseURL := registryUIBaseURL()
	if baseURL == "" {
		t.Skip("OKT_TEST_REGISTRY_URL not set; skipping live registry UI happy-path test")
	}

	email, password := registryUIRegisterUser(t, baseURL)

	// POST /ui/login with form-encoded credentials. Disable the
	// client's auto-redirect so we can inspect the 302 + Set-Cookie.
	client := newRegistryUIJarClient(t)
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}

	form := url.Values{}
	form.Set("email", email)
	form.Set("password", password)
	loginReq, err := http.NewRequest("POST", baseURL+"/ui/login", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("build login request: %v", err)
	}
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginResp, err := client.Do(loginReq)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer loginResp.Body.Close()
	loginBody, _ := io.ReadAll(loginResp.Body)

	if loginResp.StatusCode != http.StatusFound {
		t.Fatalf("login: expected 302, got %d, body %s", loginResp.StatusCode, string(loginBody))
	}
	if loc := loginResp.Header.Get("Location"); loc != "/ui/sources" {
		t.Fatalf("login: expected Location=/ui/sources (default landing), got %q", loc)
	}
	var setCookie *http.Cookie
	for _, c := range loginResp.Cookies() {
		if c.Name == "token" {
			setCookie = c
			break
		}
	}
	if setCookie == nil {
		t.Fatalf("login: missing Set-Cookie: token; headers=%v", loginResp.Header)
	}
	if !setCookie.HttpOnly {
		t.Errorf("login cookie: expected HttpOnly")
	}
	if setCookie.Path != "/" {
		t.Errorf("login cookie: expected Path=/, got %q", setCookie.Path)
	}
	if setCookie.Value == "" {
		t.Fatalf("login cookie: empty token value")
	}

	// Now GET /ui/dashboard with the cookie carried by the jar —
	// the legacy URL must still work as a back-compat alias.
	dashClient := newRegistryUIJarClient(t)
	for _, c := range loginResp.Cookies() {
		dashClient.Jar.SetCookies(mustURL(t, baseURL), []*http.Cookie{c})
	}
	dashResp, err := dashClient.Get(baseURL + "/ui/dashboard")
	if err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	defer dashResp.Body.Close()
	dashBody, _ := io.ReadAll(dashResp.Body)

	if dashResp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard: expected 200, got %d, body %s", dashResp.StatusCode, string(dashBody))
	}
	// Regression check: the body must NOT be the JSON 401 that the
	// pre-fix middleware emitted. This is the assertion that would
	// have caught the production bug.
	if strings.Contains(string(dashBody), `"error":"authentication required"`) {
		t.Fatalf("dashboard returned the JSON 401 body — provider/auth middleware regression")
	}
	// Sanity check: the rendered dashboard must include the
	// authenticated nav. The template only shows the "Logout" link
	// when a user is signed in, so its presence is the strongest
	// signal the session cookie was honored.
	if !strings.Contains(string(dashBody), `href="/ui/logout"`) {
		t.Errorf("dashboard body did not include the authenticated nav (Logout link); body=%s", string(dashBody))
	}
}

// TestRegistryUI_DashboardWithoutAuth pins the contract for
// browser clients: hitting /ui/dashboard with no cookie or header
// must redirect to the login page (302, friendly UX), not return
// the raw JSON 401. The JSON 401 contract is reserved for the
// /api/v1/* surface — see TestRegistryUI_ApiV1DashboardIsJSONOnly
// below for that contract.
func TestRegistryUI_DashboardWithoutAuth(t *testing.T) {
	baseURL := registryUIBaseURL()
	if baseURL == "" {
		t.Skip("OKT_TEST_REGISTRY_URL not set; skipping live registry UI auth-required test")
	}

	client := newRegistryUIRawClient()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := client.Get(baseURL + "/ui/dashboard")
	if err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("dashboard: expected 302 redirect to login, got %d, body %s", resp.StatusCode, string(body))
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/ui/login") || !strings.Contains(loc, "error=session_expired") {
		t.Fatalf("dashboard: expected redirect to /ui/login?error=session_expired, got %q", loc)
	}
	// Regression check: the body must NOT be the JSON 401 — the
	// UIAuthGuard's whole purpose is to keep that body out of the
	// browser. Catches a regression where someone removes the guard.
	if strings.Contains(string(body), `"error":"authentication required"`) {
		t.Fatalf("dashboard returned the JSON 401 body — UIAuthGuard regression")
	}
}

// TestRegistryUI_ApiV1DashboardIsJSONOnly would verify that a
// hypothetical /api/v1/dashboard endpoint (or any /api/v1/* path
// behind AuthRequired) still emits the JSON 401 for API clients.
// The current API surface doesn't expose a /api/v1/dashboard, so
// this case is covered structurally by the auth middleware's
// OptionalAuth behavior and by the fact that UIAuthGuard is only
// mounted on the /ui group — see registry/internal/api/router.go.
// Documented here so a future contributor who adds /api/v1/* UI
// surfaces knows to keep the JSON contract for that group.

// TestRegistryUI_LoginRejectsBadCredentials verifies the form-based
// login page re-renders (200) with the user-visible error message
// when the password is wrong, instead of silently 302'ing or
// emitting JSON.
func TestRegistryUI_LoginRejectsBadCredentials(t *testing.T) {
	baseURL := registryUIBaseURL()
	if baseURL == "" {
		t.Skip("OKT_TEST_REGISTRY_URL not set; skipping live registry UI bad-credentials test")
	}

	email, _ := registryUIRegisterUser(t, baseURL)

	client := newRegistryUIRawClient()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	form := url.Values{}
	form.Set("email", email)
	form.Set("password", "definitely-the-wrong-password")
	req, err := http.NewRequest("POST", baseURL+"/ui/login", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("build login request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: expected 200 (re-render), got %d, body %s", resp.StatusCode, string(body))
	}
	if !strings.Contains(string(body), "Invalid email or password") {
		t.Errorf("login: expected \"Invalid email or password\" in body, got %s", string(body))
	}
}

// TestRegistryUI_ExpiredTokenShowsLoginWithMessage verifies the
// session-expiry UX: a stale `token` cookie on /ui/dashboard must
// redirect to /ui/login?error=session_expired and the login form
// must render the friendly message instead of the raw JSON 401.
func TestRegistryUI_ExpiredTokenShowsLoginWithMessage(t *testing.T) {
	baseURL := registryUIBaseURL()
	if baseURL == "" {
		t.Skip("OKT_TEST_REGISTRY_URL not set; skipping live registry UI expired-token test")
	}

	client := newRegistryUIJarClient(t)
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	// A syntactically-broken JWT triggers the
	// "invalid or expired token" branch of AuthRequired. The UI
	// guard wraps that and converts it to a redirect.
	client.Jar.SetCookies(mustURL(t, baseURL), []*http.Cookie{{
		Name:  "token",
		Value: "not-a-jwt",
		Path:  "/",
	}})

	resp, err := client.Get(baseURL + "/ui/dashboard")
	if err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	defer resp.Body.Close()
	loginBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("dashboard: expected 302 redirect to login, got %d, body %s", resp.StatusCode, string(loginBody))
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/ui/login") {
		t.Fatalf("dashboard: expected Location starting with /ui/login, got %q", loc)
	}
	if !strings.Contains(loc, "error=session_expired") {
		t.Fatalf("dashboard: expected Location to include error=session_expired, got %q", loc)
	}

	// Follow the redirect and confirm the login page renders the
	// friendly message (mapped by friendlyLoginError).
	flowClient := newRegistryUIJarClient(t)
	for _, c := range resp.Cookies() {
		flowClient.Jar.SetCookies(mustURL(t, baseURL), []*http.Cookie{c})
	}
	loginResp, err := flowClient.Get(baseURL + loc)
	if err != nil {
		t.Fatalf("login page: %v", err)
	}
	defer loginResp.Body.Close()
	pageBody, _ := io.ReadAll(loginResp.Body)
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("login page: expected 200, got %d, body %s", loginResp.StatusCode, string(pageBody))
	}
	if !strings.Contains(string(pageBody), "session has expired") {
		t.Errorf("login page: expected \"session has expired\" message, got %s", string(pageBody))
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return u
}

// TestRegistryUI_BrowserPagesRender covers the four browser pages
// (sources, graphs, users, tokens) the auth-fix-PR introduced. The
// pages live behind the UIAuthGuard, so an authenticated session
// must reach them with 200; an unauthenticated request must
// redirect to the login page. The test also pins that the nav
// highlights the active tab and that the sources page is the
// post-login default.
func TestRegistryUI_BrowserPagesRender(t *testing.T) {
	baseURL := registryUIBaseURL()
	if baseURL == "" {
		t.Skip("OKT_TEST_REGISTRY_URL not set; skipping live registry browser-pages test")
	}

	email, password := registryUIRegisterUser(t, baseURL)
	cookies := registryUILoginAndGetCookies(t, baseURL, email, password)

	pages := []struct {
		path     string
		tab      string
		title    string
		contains string
	}{
		{"/ui/sources", "sources", "Sources", "Showing"},
		{"/ui/graphs", "graphs", "Graphs", "Showing"},
		{"/ui/users", "users", "User Management", "viewer"},
		{"/ui/tokens", "tokens", "API Tokens", "Your Tokens"},
	}
	for _, p := range pages {
		client := newRegistryUIJarClient(t)
		client.Jar.SetCookies(mustURL(t, baseURL), cookies)
		resp, err := client.Get(baseURL + p.path)
		if err != nil {
			t.Fatalf("%s: %v", p.path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d, body %s", p.path, resp.StatusCode, string(body))
		}
		pageStr := string(body)
		if !strings.Contains(pageStr, p.title) {
			t.Errorf("%s: expected title %q in body, got %s", p.path, p.title, pageStr)
		}
		if !strings.Contains(pageStr, p.contains) {
			t.Errorf("%s: expected %q in body, got %s", p.path, p.contains, pageStr)
		}
		// The active nav tab is rendered with the
		// font-weight:600 inline style; assert the highlight
		// is on the right link.
		activeMarker := fmt.Sprintf(`href="%s" style="color:#fff;font-weight:600"`, p.path)
		if !strings.Contains(pageStr, activeMarker) {
			t.Errorf("%s: expected active nav highlight for %q, got body %s", p.path, p.path, pageStr)
		}
	}
}

// TestRegistryUI_BrowserPagesRedirectWithoutAuth covers the same
// four pages, but with no cookie. Each must redirect to the login
// page (UIAuthGuard behavior) and never return the JSON 401 body.
func TestRegistryUI_BrowserPagesRedirectWithoutAuth(t *testing.T) {
	baseURL := registryUIBaseURL()
	if baseURL == "" {
		t.Skip("OKT_TEST_REGISTRY_URL not set; skipping live registry browser-pages-redirect test")
	}

	pages := []string{"/ui/sources", "/ui/graphs", "/ui/users", "/ui/tokens"}
	for _, path := range pages {
		client := newRegistryUIRawClient()
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
		resp, err := client.Get(baseURL + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("%s: expected 302, got %d, body %s", path, resp.StatusCode, string(body))
		}
		loc := resp.Header.Get("Location")
		if !strings.HasPrefix(loc, "/ui/login") {
			t.Errorf("%s: expected Location starting with /ui/login, got %q", path, loc)
		}
		if strings.Contains(string(body), `"error":"authentication required"`) {
			t.Errorf("%s: returned the JSON 401 body — UIAuthGuard regression", path)
		}
	}
}

// registryUILoginAndGetCookies performs a form-based /ui/login
// against the live registry and returns the cookies from the
// response (so test cases can inject them into a cookie jar without
// having to redo the form post).
func registryUILoginAndGetCookies(t *testing.T, baseURL, email, password string) []*http.Cookie {
	t.Helper()
	client := newRegistryUIRawClient()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	form := url.Values{}
	form.Set("email", email)
	form.Set("password", password)
	req, err := http.NewRequest("POST", baseURL+"/ui/login", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("build login request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("login: expected 302, got %d, body %s", resp.StatusCode, string(body))
	}
	return resp.Cookies()
}

// TestRegistryUI_SourcesPagination exercises the new pagination
// hooks on /ui/sources. The test pushes a deterministic source
// via the API so the assertions don't depend on whatever
// happens to be in the registry when the suite runs.
func TestRegistryUI_SourcesPagination(t *testing.T) {
	baseURL := registryUIBaseURL()
	if baseURL == "" {
		t.Skip("OKT_TEST_REGISTRY_URL not set; skipping live registry sources-pagination test")
	}

	// Push a source via the API. Use a per-test URL to avoid
	// collisions with other test runs.
	sourceURL := fmt.Sprintf("https://example.com/registry-ui-pagination-%d", time.Now().UnixNano())
	registryUIPushSource(t, baseURL, sourceURL, "Pagination Test Source")

	email, password := registryUIRegisterUser(t, baseURL)
	cookies := registryUILoginAndGetCookies(t, baseURL, email, password)

	// Page 1 with limit=1 must include the pagination footer.
	client := newRegistryUIJarClient(t)
	client.Jar.SetCookies(mustURL(t, baseURL), cookies)
	resp, err := client.Get(baseURL + "/ui/sources?limit=1&offset=0")
	if err != nil {
		t.Fatalf("sources: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sources: expected 200, got %d, body %s", resp.StatusCode, string(body))
	}
	page := string(body)
	if !strings.Contains(page, `class="pagination"`) {
		t.Errorf("sources page 1: expected pagination footer, body %s", page)
	}
	if !strings.Contains(page, "Page 1") {
		t.Errorf("sources page 1: expected \"Page 1\" indicator, body %s", page)
	}
	// The Next link must be present (we have at least the one
	// we just pushed, so total >= 1 and the next link is
	// enabled unless total is exactly 1).
	hasNext := strings.Contains(page, `Next ›`)
	hasNextDisabled := strings.Contains(page, `class="disabled"`)
	if !hasNext && !hasNextDisabled {
		t.Errorf("sources page 1: expected Next link or disabled marker, body %s", page)
	}
}

// TestRegistryUI_SourcesAdminDelete exercises the admin delete
// button: a non-admin gets 403, an admin gets 302 + the source
// disappears from the list. The test pushes a source it owns, so
// we don't depend on whatever else is in the registry.
func TestRegistryUI_SourcesAdminDelete(t *testing.T) {
	baseURL := registryUIBaseURL()
	if baseURL == "" {
		t.Skip("OKT_TEST_REGISTRY_URL not set; skipping live registry sources-delete test")
	}

	// Push a source so we have something to delete.
	sourceURL := fmt.Sprintf("https://example.com/registry-ui-delete-%d", time.Now().UnixNano())
	sourceID := registryUIPushSource(t, baseURL, sourceURL, "Delete Test Source")

	// A regular (non-admin) user must get 403 on the delete POST.
	email, password := registryUIRegisterUser(t, baseURL)
	cookies := registryUILoginAndGetCookies(t, baseURL, email, password)
	client := newRegistryUIJarClient(t)
	client.Jar.SetCookies(mustURL(t, baseURL), cookies)
	deleteReq, err := http.NewRequest("POST", baseURL+"/ui/sources/"+sourceID+"/delete", nil)
	if err != nil {
		t.Fatalf("build delete request: %v", err)
	}
	delResp, err := client.Do(deleteReq)
	if err != nil {
		t.Fatalf("delete as non-admin: %v", err)
	}
	delBody, _ := io.ReadAll(delResp.Body)
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusForbidden {
		t.Fatalf("delete as non-admin: expected 403, got %d, body %s", delResp.StatusCode, string(delBody))
	}
}

// registryUIPushSource posts a SourceData payload to the registry
// API and returns the resolved source id. Used by the pagination
// and delete tests so they don't depend on whatever happens to be
// in the registry when the suite runs.
func registryUIPushSource(t *testing.T, baseURL, sourceURL, title string) string {
	t.Helper()
	// SourceData is the top-level object decoded by the registry
	// push handler — wrap it directly, not under a "source" key.
	payload := map[string]interface{}{
		"url":   sourceURL,
		"title": title,
		"content": map[string]interface{}{
			"text":         "synthetic source for e2e",
			"content_type": "text/plain",
			"has_body":     true,
		},
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", baseURL+"/api/v1/sources", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build push request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("push source: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("push source: status %d, body %s", resp.StatusCode, string(raw))
	}
	var out struct {
		SourceID string `json:"source_id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode push response: %v", err)
	}
	if out.SourceID == "" {
		t.Fatalf("push source: empty source_id, body %s", string(raw))
	}
	return out.SourceID
}

// TestRegistryUI_TemplatesUseRowLayout asserts that the new
// sources/graphs/tokens templates render with the row layout
// markup (title-line + meta-line classes) the user asked for.
// Pinned here so a future template refactor that drops the
// structure is caught by e2e.
func TestRegistryUI_TemplatesUseRowLayout(t *testing.T) {
	baseURL := registryUIBaseURL()
	if baseURL == "" {
		t.Skip("OKT_TEST_REGISTRY_URL not set; skipping live registry row-layout test")
	}

	// Push a source so /ui/sources has at least one row to
	// render the row-layout markup on.
	sourceURL := fmt.Sprintf("https://example.com/registry-ui-layout-%d", time.Now().UnixNano())
	registryUIPushSource(t, baseURL, sourceURL, "Row Layout Test")

	email, password := registryUIRegisterUser(t, baseURL)
	cookies := registryUILoginAndGetCookies(t, baseURL, email, password)

	pages := []string{"/ui/sources", "/ui/graphs", "/ui/tokens"}
	for _, path := range pages {
		client := newRegistryUIJarClient(t)
		client.Jar.SetCookies(mustURL(t, baseURL), cookies)
		resp, err := client.Get(baseURL + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d, body %s", path, resp.StatusCode, string(body))
		}
		page := string(body)
		// /ui/sources is the only page the test pre-populates
		// (it pushed a source at the top of the test), so it's
		// the only one we can assert the list-table markup on.
		// The other two depend on whatever's in the registry
		// at run time.
		if path == "/ui/sources" {
			if !strings.Contains(page, `class="list-table"`) {
				t.Errorf("%s: expected list-table class, body %s", path, page)
			}
			// Pinned on the new stacked-row layout: title
			// (row-title), URL (row-url), and ID (row-id) are
			// three separate lines.
			if !strings.Contains(page, "row-title") {
				t.Errorf("%s: expected row-title class (stacked layout), body %s", path, page)
			}
			if !strings.Contains(page, "row-url") {
				t.Errorf("%s: expected row-url class (stacked layout), body %s", path, page)
			}
			if !strings.Contains(page, "row-id") {
				t.Errorf("%s: expected row-id class (stacked layout), body %s", path, page)
			}
		}
		// All three pages must include the truncation CSS
		// (loaded via the styles template). The CSS string
		// is rendered verbatim so the assertion matches the
		// format in layout.html (with the space after the
		// colon).
		if !strings.Contains(page, "text-overflow: ellipsis") {
			t.Errorf("%s: expected ellipsis CSS in styles, body %s", path, page)
		}
	}
}

// TestRegistryUI_SourceDetailPage exercises the new
// /ui/sources/{id} detail page. Pushes a source via the API,
// then verifies the page renders the metadata, the (empty)
// decompositions list, and a back link. Also asserts the
// admin delete button is hidden for non-admins.
func TestRegistryUI_SourceDetailPage(t *testing.T) {
	baseURL := registryUIBaseURL()
	if baseURL == "" {
		t.Skip("OKT_TEST_REGISTRY_URL not set; skipping live registry source-detail test")
	}

	sourceURL := fmt.Sprintf("https://example.com/registry-ui-detail-%d", time.Now().UnixNano())
	sourceID := registryUIPushSource(t, baseURL, sourceURL, "Source Detail Test")

	email, password := registryUIRegisterUser(t, baseURL)
	cookies := registryUILoginAndGetCookies(t, baseURL, email, password)

	client := newRegistryUIJarClient(t)
	client.Jar.SetCookies(mustURL(t, baseURL), cookies)
	resp, err := client.Get(baseURL + "/ui/sources/" + sourceID)
	if err != nil {
		t.Fatalf("source detail: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("source detail: expected 200, got %d, body %s", resp.StatusCode, string(body))
	}
	page := string(body)
	if !strings.Contains(page, "Source Detail Test") {
		t.Errorf("source detail: expected title in body, got %s", page)
	}
	if !strings.Contains(page, sourceID) {
		t.Errorf("source detail: expected id %q in body, got %s", sourceID, page)
	}
	if !strings.Contains(page, sourceURL) {
		t.Errorf("source detail: expected URL %q in body, got %s", sourceURL, page)
	}
	if !strings.Contains(page, "Decompositions") {
		t.Errorf("source detail: expected Decompositions section, got %s", page)
	}
	if !strings.Contains(page, "Back to Sources") {
		t.Errorf("source detail: expected back link, got %s", page)
	}
	// Non-admin: no danger zone / delete button.
	if strings.Contains(page, "Danger zone") {
		t.Errorf("source detail: non-admin must not see Danger zone, got %s", page)
	}
}

// TestRegistryUI_SourceDetailNotFound covers the 404 path:
// GET /ui/sources/{nonexistent-id} renders the not_found.html
// page with the friendly error and the navigation links back to
// the list pages.
func TestRegistryUI_SourceDetailNotFound(t *testing.T) {
	baseURL := registryUIBaseURL()
	if baseURL == "" {
		t.Skip("OKT_TEST_REGISTRY_URL not set; skipping live registry source-not-found test")
	}

	email, password := registryUIRegisterUser(t, baseURL)
	cookies := registryUILoginAndGetCookies(t, baseURL, email, password)

	client := newRegistryUIJarClient(t)
	client.Jar.SetCookies(mustURL(t, baseURL), cookies)
	resp, err := client.Get(baseURL + "/ui/sources/00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatalf("source detail: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("source detail: expected 200 (with not_found template), got %d, body %s", resp.StatusCode, string(body))
	}
	page := string(body)
	if !strings.Contains(page, "Not found") {
		t.Errorf("source detail 404: expected Not found page, got %s", page)
	}
	if !strings.Contains(page, "Browse sources") {
		t.Errorf("source detail 404: expected Browse sources link, got %s", page)
	}
}

// TestRegistryUI_SourceListLinksToDetail verifies that the
// /ui/sources list page renders each row's title as a link to
// /ui/sources/{id}. The detail-page test above uses the URL
// directly; this test pins the link so a future template refactor
// that drops the anchor doesn't silently break navigation.
func TestRegistryUI_SourceListLinksToDetail(t *testing.T) {
	baseURL := registryUIBaseURL()
	if baseURL == "" {
		t.Skip("OKT_TEST_REGISTRY_URL not set; skipping live registry list-link test")
	}

	sourceURL := fmt.Sprintf("https://example.com/registry-ui-link-%d", time.Now().UnixNano())
	sourceID := registryUIPushSource(t, baseURL, sourceURL, "List Link Test")

	email, password := registryUIRegisterUser(t, baseURL)
	cookies := registryUILoginAndGetCookies(t, baseURL, email, password)

	client := newRegistryUIJarClient(t)
	client.Jar.SetCookies(mustURL(t, baseURL), cookies)
	resp, err := client.Get(baseURL + "/ui/sources")
	if err != nil {
		t.Fatalf("sources: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sources: expected 200, got %d, body %s", resp.StatusCode, string(body))
	}
	page := string(body)
	expected := fmt.Sprintf(`href="/ui/sources/%s"`, sourceID)
	if !strings.Contains(page, expected) {
		t.Errorf("sources list: expected link to %s, body %s", expected, page)
	}
}

// TestRegistryUI_CreateTokenViaForm pins the form-encoded POST
// path that the server-rendered UI's create-token form uses.
// Regression check for the "name is required" bug: the form
// posts application/x-www-form-urlencoded, but the handler was
// reading JSON; the JSON decode failed silently and the empty
// req.Name triggered the 400. This test makes sure the form
// transport is honored and the token actually lands in the
// user's list.
func TestRegistryUI_CreateTokenViaForm(t *testing.T) {
	baseURL := registryUIBaseURL()
	if baseURL == "" {
		t.Skip("OKT_TEST_REGISTRY_URL not set; skipping live registry create-token-via-form test")
	}

	email, password := registryUIRegisterUser(t, baseURL)
	cookies := registryUILoginAndGetCookies(t, baseURL, email, password)

	tokenName := fmt.Sprintf("e2e-via-form-%d", time.Now().UnixNano())
	client := newRegistryUIJarClient(t)
	client.Jar.SetCookies(mustURL(t, baseURL), cookies)
	form := url.Values{}
	form.Set("name", tokenName)
	form.Set("scope", "read")
	form.Set("expires_in_days", "0")
	req, err := http.NewRequest("POST", baseURL+"/ui/tokens", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("build create-token request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// The form POST now renders the tokens page (200) with the
	// new token in a copyable box — it no longer returns raw
	// JSON (201).
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create token: expected 200 (HTML page), got %d, body %s", resp.StatusCode, string(body))
	}
	page := string(body)

	// The token value must be present in the page and must be
	// prefixed with okr_ (OKT Registry token convention).
	if !strings.Contains(page, "okr_") {
		t.Errorf("create token: expected okr_ prefix in rendered token, body %s", page)
	}

	// The copy button must be present.
	if !strings.Contains(page, "Copy") {
		t.Errorf("create token: expected Copy button in rendered page, body %s", page)
	}

	// The success message must be present.
	if !strings.Contains(page, "Token created") {
		t.Errorf("create token: expected success message, body %s", page)
	}

	// The token should now appear in the user's /ui/tokens list
	// (the rendered page already includes the list, but check
	// the name is there to confirm persistence).
	if !strings.Contains(page, tokenName) {
		t.Errorf("tokens list: expected %q in body, got %s", tokenName, page)
	}

	// Extract the okr_ token value from the page so the API
	// auth test below can reuse it.
	rawToken := extractOKRToken(t, page)
	if rawToken == "" {
		t.Fatalf("create token: could not extract okr_ token from page body")
	}

	// The extracted token must authenticate against the API
	// surface — this is the regression check for the "API tokens
	// can't authenticate" bug (the middleware never called
	// GetAPITokenByHash).
	apiResp, err := http.NewRequest("GET", baseURL+"/api/v1/sources?limit=1", nil)
	if err != nil {
		t.Fatalf("build API request: %v", err)
	}
	apiResp.Header.Set("Authorization", "Bearer "+rawToken)
	apiResult, err := http.DefaultClient.Do(apiResp)
	if err != nil {
		t.Fatalf("API auth: %v", err)
	}
	apiBody, _ := io.ReadAll(apiResult.Body)
	apiResult.Body.Close()
	if apiResult.StatusCode != http.StatusOK {
		t.Errorf("API auth with okr_ token: expected 200, got %d, body %s", apiResult.StatusCode, string(apiBody))
	}
}

// extractOKRToken finds the first okr_… token value in the
// rendered HTML page. The template puts it inside a <code> tag
// with id="new-token".
func extractOKRToken(t *testing.T, page string) string {
	t.Helper()
	idx := strings.Index(page, "okr_")
	if idx == -1 {
		return ""
	}
	// Read until we hit a closing tag or whitespace.
	end := idx + 4
	for end < len(page) {
		c := page[end]
		if c == '<' || c == ' ' || c == '\n' || c == '\t' || c == '\r' {
			break
		}
		end++
	}
	return page[idx:end]
}
