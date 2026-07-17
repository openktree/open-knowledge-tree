package handler

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

type AI struct {
	Providers         map[string]ai.AIProvider
	EmbeddingProvider ai.EmbeddingProvider
	EmbeddingCfg      config.EmbeddingConfig
	Store             *store.Queries
}

func NewAI(providers map[string]ai.AIProvider, embeddingProvider ai.EmbeddingProvider, embeddingCfg config.EmbeddingConfig, queries *store.Queries) *AI {
	return &AI{
		Providers:         providers,
		EmbeddingProvider: embeddingProvider,
		EmbeddingCfg:      embeddingCfg,
		Store:             queries,
	}
}

func (a *AI) ListProviders(w http.ResponseWriter, r *http.Request) {
	providers := []map[string]interface{}{}
	for id, p := range a.Providers {
		d := p.Describe()
		providers = append(providers, map[string]interface{}{
			"id":          id,
			"name":        d.Name,
			"description": d.Description,
			"requires":    d.Requires,
			"configured":  d.Configured,
			"models":      d.Models,
			"timeout":     d.Timeout,
			"notes":       d.Notes,
		})
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"providers": providers,
	})
}

// ListEmbeddingProviders surfaces the embedding-side configuration
// that the embed_facts worker reads. The endpoint returns two
// sections:
//
//   - `active`: the scalar EmbeddingConfig (provider / model /
//     dimensions / configured). Operators read this to know which
//     model vectorizes their facts today; switching model or
//     dimensions requires re-embedding existing facts (Qdrant
//     collection must be recreated), so the catalog is intentionally
//     not editable from the UI.
//   - `providers`: every AI provider that type-asserts to
//     ai.EmbeddingProvider (so chat-only providers like ollama_cloud
//     are excluded), each with its Describe() metadata and an
//     `embedding_capable: true` flag. The UI renders one card per
//     entry so an operator can see which providers *could* embed if
//     reconfigured.
//
// `active.configured` is true only when the named provider resolved
// to a non-nil EmbeddingProvider at boot (the wiring in
// cmd/app/api.go type-asserts the named AI provider; a mismatched
// name or a chat-only provider leaves it nil and logs a warning).
func (a *AI) ListEmbeddingProviders(w http.ResponseWriter, r *http.Request) {
	active := map[string]interface{}{
		"provider":   a.EmbeddingCfg.Provider,
		"model":      a.EmbeddingCfg.Model,
		"dimensions": a.EmbeddingCfg.Dimensions,
		"configured": a.EmbeddingProvider != nil,
	}

	// Walk the chat-provider map and keep only entries that also
	// implement ai.EmbeddingProvider. Sort by id for stable UI
	// ordering — map iteration order is non-deterministic in Go.
	type embProvider struct {
		ID                string                 `json:"id"`
		Name              string                 `json:"name"`
		Description       string                 `json:"description"`
		Requires          string                 `json:"requires"`
		Configured        bool                   `json:"configured"`
		EmbeddingCapable  bool                   `json:"embedding_capable"`
		Timeout           string                 `json:"timeout"`
		Notes             string                 `json:"notes"`
	}
	providers := []embProvider{}
	for id, p := range a.Providers {
		if _, ok := p.(ai.EmbeddingProvider); !ok {
			continue
		}
		d := p.Describe()
		providers = append(providers, embProvider{
			ID:               id,
			Name:             d.Name,
			Description:      d.Description,
			Requires:         d.Requires,
			Configured:       d.Configured,
			EmbeddingCapable: true,
			Timeout:          d.Timeout,
			Notes:            d.Notes,
		})
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].ID < providers[j].ID })

	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"active":    active,
		"providers": providers,
	})
}

func (a *AI) Chat(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")

	p, ok := a.Providers[provider]
	if !ok || p == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "ai provider not available: "+provider)
		return
	}

	var req ai.ChatRequest
	if err := httputil.DecodeBody(r, &req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// Fold the optional JSON body fields into the Attribution
	// the tracking helper reads. The interactive endpoint accepts
	// repository_id / source_id so the dashboard can attribute
	// interactive calls; background tasks set Attribution directly
	// on the Go struct.
	req.Attribution.RepositoryID = req.RepositoryID
	req.Attribution.SourceID = req.SourceID
	if req.Model == "" {
		httputil.WriteError(w, http.StatusBadRequest, "model is required")
		return
	}
	if len(req.Messages) == 0 {
		httputil.WriteError(w, http.StatusBadRequest, "messages is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	resp, err := p.Chat(ctx, a.Store.DB(), req)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	httputil.WriteJSON(w, http.StatusOK, resp)
}
