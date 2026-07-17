package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestEmbedBatchRecursive_HalvesOnEmptyData verifies the auto-halve
// retry: when OpenRouter returns 200 with an empty `data` array, the
// client splits the batch in half and retries until a batch succeeds.
// The test server returns empty data for inputs containing "big" and
// valid vectors for everything else, so a batch of 4 with one "big"
// input must be halved until the "big" input is alone (single-input
// batch that still returns no data is a real error).
func TestEmbedBatchRecursive_HalvesOnEmptyData(t *testing.T) {
	// Track how many requests hit the server.
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req openRouterEmbedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Any input containing "big" triggers the empty-data
		// response (simulates the model's token limit).
		hasBig := false
		for _, in := range req.Input {
			if len(in) >= 3 && in[:3] == "big" {
				hasBig = true
				break
			}
		}
		if hasBig && len(req.Input) > 1 {
			// Empty data — the limit-exceeded signal.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"model":"test","data":[],"usage":{"prompt_tokens":0,"total_tokens":0}}`))
			return
		}
		if hasBig {
			// Single "big" input still returns no data — real error.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"model":"test","data":[],"usage":{"prompt_tokens":0,"total_tokens":0}}`))
			return
		}
		// Normal inputs get valid 2-dim vectors.
		type datum struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		data := make([]datum, len(req.Input))
		for i := range req.Input {
			data[i] = datum{Index: i, Embedding: []float32{float32(i), float32(i + 1)}}
		}
		resp := map[string]interface{}{
			"model": "test",
			"data":  data,
			"usage": map[string]int{"prompt_tokens": len(req.Input), "total_tokens": len(req.Input)},
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := &OpenRouterProvider{
		apiKey:          "test-key",
		httpClient:      srv.Client(),
		embedBatchSize:  4,
	}
	// Override the URL by patching the package-level const is not
	// possible, so we point the httpClient at the test server via
	// a custom transport that rewrites the URL.
	p.httpClient.Transport = &urlRewriter{base: srv.URL, original: openrouterEmbeddingsURL}

	// 3 normal inputs — should succeed on the first batch.
	resp, err := p.embedBatchRecursive(context.Background(), "test-model", []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("expected success for all-normal batch, got: %v", err)
	}
	if len(resp.Embeddings) != 3 {
		t.Fatalf("expected 3 embeddings, got %d", len(resp.Embeddings))
	}

	// 4 inputs, one "big" — halving should isolate "big" as a
	// single-input batch and surface the error for it. The other
	// 3 should succeed. The overall call returns an error because
	// one half ("big") fails.
	calls = 0
	_, err = p.embedBatchRecursive(context.Background(), "test-model", []string{"a", "b", "big", "d"})
	if err == nil {
		t.Fatal("expected error because 'big' input can't be embedded even alone")
	}
	// Verify multiple calls happened (halving produced sub-batches).
	if calls < 3 {
		t.Errorf("expected at least 3 HTTP calls from halving, got %d", calls)
	}
}

// TestEmbedBatchRecursive_AllNormalSucceeds verifies that a batch
// of normal inputs (no limit-exceeded) succeeds in a single request
// with no halving.
func TestEmbedBatchRecursive_AllNormalSucceeds(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req openRouterEmbedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		type datum struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		data := make([]datum, len(req.Input))
		for i := range req.Input {
			data[i] = datum{Index: i, Embedding: []float32{float32(i)}}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"model": "test",
			"data":  data,
			"usage": map[string]int{"prompt_tokens": len(req.Input), "total_tokens": len(req.Input)},
		})
	}))
	defer srv.Close()

	p := &OpenRouterProvider{
		apiKey:          "test-key",
		httpClient:      srv.Client(),
		embedBatchSize:  64,
	}
	p.httpClient.Transport = &urlRewriter{base: srv.URL, original: openrouterEmbeddingsURL}

	resp, err := p.embedBatchRecursive(context.Background(), "m", []string{"x", "y", "z", "w"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Embeddings) != 4 {
		t.Fatalf("expected 4 embeddings, got %d", len(resp.Embeddings))
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 HTTP call (no halving), got %d", calls)
	}
	if resp.Usage.PromptTokens != 4 {
		t.Errorf("expected prompt_tokens=4, got %d", resp.Usage.PromptTokens)
	}
}

// TestEmbed_HalvesAcrossBatches verifies the top-level Embed method
// chunks inputs into embedBatchSize and halves any chunk that
// returns empty data, concatenating all results in order.
func TestEmbed_HalvesAcrossBatches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openRouterEmbedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		// "overflow" triggers empty-data when in a multi-input batch.
		overflow := false
		for _, in := range req.Input {
			if in == "overflow" {
				overflow = true
			}
		}
		if overflow && len(req.Input) > 1 {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"model":"m","data":[],"usage":{"prompt_tokens":0,"total_tokens":0}}`))
			return
		}
		if overflow {
			// Single "overflow" input returns valid data (simulates
			// the limit being input-count, not single-input tokens).
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"model": "m",
				"data":  []map[string]interface{}{{"index": 0, "embedding": []float32{99}}},
				"usage": map[string]int{"prompt_tokens": 1, "total_tokens": 1},
			})
			return
		}
		type datum struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		data := make([]datum, len(req.Input))
		for i := range req.Input {
			data[i] = datum{Index: i, Embedding: []float32{float32(i)}}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"model": "m",
			"data":  data,
			"usage": map[string]int{"prompt_tokens": len(req.Input), "total_tokens": len(req.Input)},
		})
	}))
	defer srv.Close()

	p := &OpenRouterProvider{
		apiKey:          "k",
		httpClient:      srv.Client(),
		embedBatchSize:  4,
	}
	p.httpClient.Transport = &urlRewriter{base: srv.URL, original: openrouterEmbeddingsURL}

	// 6 inputs with embedBatchSize=4 → batches [0:4] and [4:6].
	// First batch contains "overflow" → halved until it's alone.
	inputs := []string{"a", "b", "overflow", "c", "d", "e"}
	resp, err := p.Embed(context.Background(), nil, EmbeddingRequest{Model: "m", Inputs: inputs})
	if err != nil {
		t.Fatalf("Embed failed: %v", err)
	}
	if len(resp.Embeddings) != 6 {
		t.Fatalf("expected 6 embeddings, got %d", len(resp.Embeddings))
	}
}

// urlRewriter rewrites requests targeting `original` to go to `base`
// instead, so we can point the provider's httpClient at a test server
// without changing the package-level URL const.
type urlRewriter struct {
	base     string
	original string
}

func (u *urlRewriter) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.String() == u.original {
		req.URL.Scheme = "http"
		req.URL.Host = ""
		// Rebuild against base.
		req2 := req.Clone(req.Context())
		req2.URL.Scheme = "http"
		// Extract host:port from base.
		host := u.base
		if len(host) >= 7 && host[:7] == "http://" {
			host = host[7:]
		}
		req2.URL.Host = host
		req2.RequestURI = req.URL.Path
		return http.DefaultTransport.RoundTrip(req2)
	}
	return http.DefaultTransport.RoundTrip(req)
}