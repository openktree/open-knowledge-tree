//go:build e2e

package e2e_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
)

func TestAuthRegister(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	resp, body := client.register("test@example.com", "password123", "Test User")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, body)
	}

	var user struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	json.Unmarshal(body, &user)

	if user.Email != "test@example.com" {
		t.Fatalf("expected test@example.com, got %s", user.Email)
	}
	if user.ID == "" {
		t.Fatal("expected non-empty user ID")
	}
}

func TestAuthRegisterMissingFields(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	resp, body := client.register("incomplete@example.com", "", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}
}

func TestAuthRegisterDuplicateEmail(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	client.register("dup@example.com", "password123", "First")
	resp, body := client.register("dup@example.com", "password456", "Second")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", resp.StatusCode, body)
	}
}

func TestAuthLogin(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	client.register("login-test@example.com", "password123", "Login Test")

	resp, body := client.login("login-test@example.com", "password123")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var loginResp struct {
		Token       string `json:"token"`
		JWT         string `json:"jwt"`
		DisplayName string `json:"display_name"`
		Email       string `json:"email"`
	}
	json.Unmarshal(body, &loginResp)

	if loginResp.Token == "" {
		t.Fatal("expected non-empty session token")
	}
	if loginResp.Email != "login-test@example.com" {
		t.Fatalf("expected login-test@example.com, got %s", loginResp.Email)
	}
	if loginResp.DisplayName != "Login Test" {
		t.Fatalf("expected 'Login Test', got %s", loginResp.DisplayName)
	}
}

func TestAuthLoginInvalidPassword(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	client.register("bad-pass@example.com", "password123", "Bad Pass")

	resp, body := client.login("bad-pass@example.com", "wrongpassword")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", resp.StatusCode, body)
	}
}

func TestAuthLoginNonexistentUser(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	resp, body := client.login("nobody@example.com", "password123")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", resp.StatusCode, body)
	}
}

func TestAuthLogout(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	client.register("logout@example.com", "password123", "Logout Test")

	_, loginBody := client.login("logout@example.com", "password123")
	var loginResp struct {
		Token string `json:"token"`
	}
	json.Unmarshal(loginBody, &loginResp)
	client.token = loginResp.Token

	resp, body := client.logout()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	resp, body = client.logout()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for double logout, got %d: %s", resp.StatusCode, body)
	}
}

func TestAuthRefresh(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	client.register("refresh@example.com", "password123", "Refresh Test")

	_, loginBody := client.login("refresh@example.com", "password123")
	var loginResp struct {
		Token string `json:"token"`
	}
	json.Unmarshal(loginBody, &loginResp)
	client.token = loginResp.Token

	resp, body := client.refresh()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var refreshResp struct {
		Token string `json:"token"`
	}
	json.Unmarshal(body, &refreshResp)
	if refreshResp.Token == "" {
		t.Fatal("expected non-empty refreshed token")
	}
	if refreshResp.Token == loginResp.Token {
		t.Fatal("expected a different token after refresh")
	}
}

func TestAuthRefreshInvalidToken(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)
	client.token = "deadbeefdeadbeefdeadbeefdeadbeef"

	resp, _ := client.refresh()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}
