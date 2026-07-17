//go:build e2e

package e2e_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

type authClient struct {
	baseURL    string
	httpClient *http.Client
	token      string
}

func newAuthClient(baseURL string) *authClient {
	return &authClient{
		baseURL:    baseURL,
		httpClient: &http.Client{},
	}
}

func (c *authClient) register(email, password, displayName string) (*http.Response, []byte) {
	body, _ := json.Marshal(map[string]string{
		"email":        email,
		"password":     password,
		"display_name": displayName,
	})
	resp, err := c.httpClient.Post(c.baseURL+"/api/v1/auth/register", "application/json", bytes.NewReader(body))
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

func (c *authClient) login(email, password string) (*http.Response, []byte) {
	body, _ := json.Marshal(map[string]string{
		"email":    email,
		"password": password,
	})
	resp, err := c.httpClient.Post(c.baseURL+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

func (c *authClient) logout() (*http.Response, []byte) {
	req, _ := http.NewRequest("POST", c.baseURL+"/api/v1/auth/logout", nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

func (c *authClient) refresh() (*http.Response, []byte) {
	req, _ := http.NewRequest("POST", c.baseURL+"/api/v1/auth/refresh", nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

func (c *authClient) do(method, path string, body []byte) (*http.Response, []byte) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, c.baseURL+path, bodyReader)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

// doWithHeaders is like do but lets the caller set extra headers on
// the request (e.g. X-Repository-ID to simulate the frontend's
// repository-scoped calls).
func (c *authClient) doWithHeaders(method, path string, body []byte, headers map[string]string) (*http.Response, []byte) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, c.baseURL+path, bodyReader)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

// doMultipart sends a multipart/form-data request. The caller builds
// the body via a bytes.Buffer + mime/multipart.Writer and passes the
// resulting Content-Type (which includes the boundary). Used by the
// upload-source e2e tests.
func (c *authClient) doMultipart(method, path, contentType string, body []byte) (*http.Response, []byte) {
	req, _ := http.NewRequest(method, c.baseURL+path, bytes.NewReader(body))
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}
