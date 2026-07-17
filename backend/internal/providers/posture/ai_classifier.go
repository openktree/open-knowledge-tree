package posture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// AIClassifier is the LLM-backed Classifier. It wraps an ai.AIProvider
// (the same multi-provider gateway the decomposition/summarization
// providers use) plus a default model id, builds the posture
// classification prompt, calls Chat with retry, and parses the JSON
// array the model returns into []Classification. The provider records
// token usage into okt_system.ai_usage via the ai.Attribution the
// worker passes through.
type AIClassifier struct {
	aiProvider ai.AIProvider
	model      string
}

// NewAIClassifier constructs the classifier. aiProvider may be nil
// (Configured() returns false); model may be empty (Configured()
// returns false). The worker checks Configured() before calling
// Classify so a deployment without a chat provider falls back to the
// keep-all path without a panic.
func NewAIClassifier(aiProvider ai.AIProvider, model string) *AIClassifier {
	return &AIClassifier{aiProvider: aiProvider, model: model}
}

// Configured reports whether the classifier is ready to run: a non-nil
// AIProvider whose Describe().Configured is true and a non-empty model.
func (c *AIClassifier) Configured() bool {
	if c == nil || c.aiProvider == nil || c.model == "" {
		return false
	}
	return c.aiProvider.Describe().Configured
}

func (c *AIClassifier) Describe() ProviderDescription {
	name, configured := "(none)", false
	if c != nil && c.aiProvider != nil {
		desc := c.aiProvider.Describe()
		name = desc.Name
		configured = desc.Configured && c.model != ""
	}
	return ProviderDescription{
		Name:        "AI autocite posture classifier",
		Description: "Labels each (sentence, candidate fact) pair as related / supports / contradicts / irrelevant so the annotate_report worker can drop irrelevant matches before persisting report_annotations.",
		Requires:    "providers.reports.posture_classifier.{enabled,provider,model} and the underlying AI provider's API key",
		Configured:  configured,
		Supports:    []string{"report_annotation"},
		Notes:       "Provider is " + name + ". Per-batch failures are logged and the batch falls back to keep-all (posture = NULL); the report still annotates.",
		Config: map[string]string{
			"ai_provider": name,
			"model":       c.model,
		},
	}
}

// Classify builds the prompt from req.Sentences, calls the AI
// provider's Chat, and parses the JSON array the model returns. The
// db arg is passed to the AI provider so it can record token usage
// into ai_usage (mirrors every other AI-backed provider); pass nil
// to skip recording (tests).
//
// The response is expected to be a JSON array of objects
//   [{"sentence_index": int, "fact_id": "uuid", "posture": "related|supports|contradicts|irrelevant"}, ...].
// Unparseable entries are dropped (logged, not fatal) so one bad row
// doesn't invalidate the whole batch. Pairs the model omits are
// treated as irrelevant (dropped) — the prompt explicitly requires
// one entry per input pair.
func (c *AIClassifier) Classify(ctx context.Context, db store.DBTX, req ClassifyRequest) ([]Classification, error) {
	if c.aiProvider == nil {
		return nil, fmt.Errorf("posture: ai provider not configured")
	}
	if len(req.Sentences) == 0 {
		return nil, nil
	}

	model := req.Model
	if model == "" {
		model = c.model
	}

	userMsg := buildUserMessage(req.Sentences)

	var taskID *string
	if req.TaskID != "" {
		taskID = &req.TaskID
	}

	chatReq := ai.ChatRequest{
		Model:    model,
		Messages: []ai.ChatMessage{{Role: "system", Content: systemPrompt}, {Role: "user", Content: userMsg}},
		TaskID:   taskID,
		Attribution: ai.Attribution{
			RepositoryID: req.Attribution.RepositoryID,
			SourceID:     req.Attribution.SourceID,
			Operation:    "report_annotation",
		},
	}
	if req.MaxTokens > 0 {
		mt := req.MaxTokens
		chatReq.MaxTokens = &mt
	}

	resp, err := retryWithBackoff(ctx, retryConfig{}, "posture_classify",
		func(callCtx context.Context) (ai.ChatResponse, error) {
			return c.aiProvider.Chat(callCtx, db, chatReq)
		},
	)
	if err != nil {
		return nil, fmt.Errorf("posture: ai chat failed: %w", err)
	}
	if len(resp.Messages) == 0 {
		return nil, nil
	}
	content := strings.TrimSpace(resp.Messages[0].Content)
	if content == "" {
		return nil, nil
	}
	content = trimCodeFences(content)

	parsed, err := parseClassifications(content, req.Sentences)
	if err != nil {
		// Don't fail the whole report on a malformed response; the
		// caller logs and falls back to keep-all for this batch.
		return nil, fmt.Errorf("posture: parsing classifier response: %w", err)
	}
	return parsed, nil
}

// systemPrompt is the fixed instruction that defines the four
// postures and the strict JSON output contract.
const systemPrompt = `You are an autocite posture classifier for a knowledge-graph annotation system.

For each (sentence, fact) pair in the user message you must assign exactly one of four postures:
- "supports": the fact provides evidence FOR the claim made in the sentence;
- "contradicts": the fact provides evidence AGAINST the claim made in the sentence;
- "related": the fact is topically relevant to the sentence but neither supports nor contradicts its claim;
- "irrelevant": the fact is NOT meaningfully related to the sentence.

You MUST output ONLY a JSON array of objects, one per input pair, with these fields:
  {"sentence_index": <int>, "fact_id": "<uuid string>", "posture": "<related|supports|contradicts|irrelevant>"}

Do not output any prose, headings, or explanations — only the JSON array. Every input pair must appear exactly once in the output.`

// buildUserMessage renders the batch as a compact JSON array of
// {sentence_index, sentence, candidates:[{fact_id, fact_text}]} so
// the model has all the context in one block and can emit a matching
// JSON array back.
func buildUserMessage(sentences []SentenceFacts) string {
	type cand struct {
		FactID   string `json:"fact_id"`
		FactText string `json:"fact_text"`
	}
	type sent struct {
		SentenceIndex int    `json:"sentence_index"`
		Sentence      string `json:"sentence"`
		Candidates    []cand `json:"candidates"`
	}
	out := make([]sent, 0, len(sentences))
	for _, s := range sentences {
		cs := make([]cand, 0, len(s.Facts))
		for _, f := range s.Facts {
			cs = append(cs, cand{FactID: f.ID.String(), FactText: f.Text})
		}
		out = append(out, sent{
			SentenceIndex: s.SentenceIndex,
			Sentence:      s.SentenceText,
			Candidates:    cs,
		})
	}
	b, _ := json.Marshal(out)
	return fmt.Sprintf(`Classify every (sentence, candidate) pair below. Output ONLY the JSON array.

%s`, string(b))
}

// parseClassifications decodes the model's JSON array response into
// []Classification. It tolerates a leading/trailing code fence
// (already stripped by the caller) and a single trailing comma.
// Entries with an unknown posture, a bad fact_id, or a
// sentence_index not in the input batch are dropped (logged). The
// function returns no error for a syntactically valid but empty
// array — the caller treats that as "all irrelevant".
func parseClassifications(content string, input []SentenceFacts) ([]Classification, error) {
	// Tolerate a single trailing comma before the closing bracket.
	content = strings.TrimRight(strings.TrimSpace(content), ",")
	if content == "" {
		return nil, nil
	}
	if !strings.HasPrefix(content, "[") {
		return nil, fmt.Errorf("response is not a JSON array: %q", truncate(content, 80))
	}

	var rows []struct {
		SentenceIndex int    `json:"sentence_index"`
		FactID        string `json:"fact_id"`
		Posture       string `json:"posture"`
	}
	if err := json.Unmarshal([]byte(content), &rows); err != nil {
		return nil, fmt.Errorf("decoding JSON array: %w (content=%q)", err, truncate(content, 120))
	}

	// Build an index of valid (sentence_index, fact_id) pairs from
	// the input batch so we can drop rows the model hallucinated.
	valid := make(map[int]map[string]bool)
	for _, s := range input {
		m := make(map[string]bool, len(s.Facts))
		for _, f := range s.Facts {
			m[f.ID.String()] = true
		}
		valid[s.SentenceIndex] = m
	}

	out := make([]Classification, 0, len(rows))
	for i, r := range rows {
		p := normalizePosture(r.Posture)
		if p == "" {
			log.Printf("posture: dropping row %d with unknown posture %q", i, r.Posture)
			continue
		}
		fid, err := uuid.Parse(r.FactID)
		if err != nil {
			log.Printf("posture: dropping row %d with bad fact_id %q", i, r.FactID)
			continue
		}
		if m, ok := valid[r.SentenceIndex]; !ok || !m[fid.String()] {
			log.Printf("posture: dropping row %d with (sentence_index=%d, fact_id=%s) not in input batch", i, r.SentenceIndex, fid)
			continue
		}
		if p == Irrelevant {
			// Drop irrelevant pairs — the worker only persists
			// related/supports/contradicts.
			continue
		}
		out = append(out, Classification{
			SentenceIndex: r.SentenceIndex,
			FactID:        fid,
			Posture:       p,
		})
	}
	return out, nil
}

// normalizePosture maps the model's posture string to the canonical
// Posture value, accepting minor variants (support/supports,
// contradict/contradicts, relate/related). Returns "" for unknown.
func normalizePosture(s string) Posture {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "related", "relate":
		return Related
	case "supports", "support":
		return Supports
	case "contradicts", "contradict":
		return Contradicts
	case "irrelevant", "irrelevent":
		return Irrelevant
	default:
		return ""
	}
}

// trimCodeFences strips a leading ``` fence the model may add around
// its JSON despite the prompt asking for raw JSON. Mirrors the
// summarization provider's trimMarkdownFences helper.
func trimCodeFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if idx := strings.IndexByte(s, '\n'); idx != -1 {
		s = s[idx+1:]
	}
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "```") {
		s = strings.TrimSuffix(s, "```")
	}
	return strings.TrimSpace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// retryConfig + retryWithBackoff mirror the summarization provider
// so the classifier rides out the same transient failures (429,
// 5xx, network) the other AI-backed providers do. Duplicated here
// to keep the posture package self-contained (no import of
// summarization internals).
type retryConfig struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	PerCallTO   time.Duration
}

var defaultRetryConfig = retryConfig{
	MaxAttempts: 4,
	BaseDelay:   2 * time.Second,
	MaxDelay:    30 * time.Second,
	PerCallTO:   180 * time.Second,
}

func retryWithBackoff[T any](
	ctx context.Context,
	cfg retryConfig,
	label string,
	fn func(ctx context.Context) (T, error),
) (T, error) {
	var zero T
	if cfg.MaxAttempts <= 0 {
		cfg = defaultRetryConfig
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = defaultRetryConfig.BaseDelay
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = defaultRetryConfig.MaxDelay
	}

	var lastErr error
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		callCtx := ctx
		var cancel context.CancelFunc
		if cfg.PerCallTO > 0 {
			callCtx, cancel = context.WithTimeout(ctx, cfg.PerCallTO)
		}
		result, err := fn(callCtx)
		if cancel != nil {
			cancel()
		}
		if err == nil {
			return result, nil
		}
		lastErr = err
		retryable, reason := classifyError(err)
		if !retryable || attempt == cfg.MaxAttempts {
			if !retryable {
				log.Printf("%s: attempt %d/%d failed (%s, permanent): %v", label, attempt, cfg.MaxAttempts, reason, err)
			} else {
				log.Printf("%s: attempt %d/%d failed (%s) — out of retries: %v", label, attempt, cfg.MaxAttempts, reason, err)
			}
			return zero, lastErr
		}
		delay := time.Duration(float64(cfg.BaseDelay) * pow2float(attempt-1))
		if delay > cfg.MaxDelay {
			delay = cfg.MaxDelay
		}
		log.Printf("%s: attempt %d/%d failed (%s); retrying in %s: %v", label, attempt, cfg.MaxAttempts, reason, delay, err)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return zero, ctx.Err()
		}
	}
	return zero, lastErr
}

func pow2float(n int) float64 {
	r := 1.0
	for i := 0; i < n; i++ {
		r *= 2
	}
	return r
}

func classifyError(err error) (retryable bool, reason string) {
	if err == nil {
		return false, ""
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false, "context cancelled/deadline"
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true, "net.Error"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true, "EOF/unexpected EOF"
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNREFUSED) {
		return true, "connection reset/pipe/refused"
	}
	msg := err.Error()
	if strings.Contains(msg, "status 429") {
		return true, "429 rate limited"
	}
	if strings.Contains(msg, "status 500") || strings.Contains(msg, "status 502") ||
		strings.Contains(msg, "status 503") || strings.Contains(msg, "status 504") {
		return true, "5xx server error"
	}
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "timeout") || strings.Contains(lower, "connection") {
		return true, "network/timeout heuristic"
	}
	return false, "permanent"
}