package ai

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// recordUsage writes one ai_usage row for a chat call. taskID,
// attr.RepositoryID, and attr.SourceID are optional (a nil *string
// and empty strings write NULL); attr.Operation defaults to
// "chat" when empty so callers that don't care keep the legacy
// behavior. Recording is best-effort: the error is ignored so a
// tracking failure never fails the AI call.
func recordUsage(ctx context.Context, db store.DBTX, model, provider string, taskID *string, attr Attribution, usage Usage) {
	queries := store.New(db)
	operation := attr.Operation
	if operation == "" {
		operation = "chat"
	}
	params := store.RecordAIUsageParams{
		Model:            model,
		Provider:         provider,
		PromptTokens:     int32(usage.PromptTokens),
		CompletionTokens: int32(usage.CompletionTokens),
		TotalTokens:      int32(usage.TotalTokens),
		Operation:        operation,
	}
	if taskID != nil {
		params.TaskID = taskID
	}
	if u, err := uuid.Parse(attr.RepositoryID); err == nil {
		params.RepositoryID = pgtype.UUID{Bytes: u, Valid: true}
	}
	if u, err := uuid.Parse(attr.SourceID); err == nil {
		params.SourceID = pgtype.UUID{Bytes: u, Valid: true}
	}
	_ = queries.RecordAIUsage(ctx, params)
}

// recordEmbeddingUsage mirrors recordUsage for embedding calls.
// Embeddings have no completion tokens, so completion_tokens is
// always 0; the prompt_tokens and total_tokens come from the
// provider's embedding usage response. attr.Operation defaults to
// "embedding" when empty. Kept as a separate helper (rather than
// folding into recordUsage) so the call sites read clearly and a
// future schema change (e.g. a dedicated ai_embedding_usage table)
// is a contained swap.
func recordEmbeddingUsage(ctx context.Context, db store.DBTX, model, provider string, taskID *string, attr Attribution, usage EmbeddingUsage) {
	queries := store.New(db)
	operation := attr.Operation
	if operation == "" {
		operation = "embedding"
	}
	params := store.RecordAIUsageParams{
		Model:            model,
		Provider:         provider,
		PromptTokens:     int32(usage.PromptTokens),
		CompletionTokens: 0,
		TotalTokens:      int32(usage.TotalTokens),
		Operation:        operation,
	}
	if taskID != nil {
		params.TaskID = taskID
	}
	if u, err := uuid.Parse(attr.RepositoryID); err == nil {
		params.RepositoryID = pgtype.UUID{Bytes: u, Valid: true}
	}
	if u, err := uuid.Parse(attr.SourceID); err == nil {
		params.SourceID = pgtype.UUID{Bytes: u, Valid: true}
	}
	_ = queries.RecordAIUsage(ctx, params)
}