package claims

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

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// AIClaimExtractor is the LLM-backed Extractor. It wraps an
// ai.AIProvider (the same multi-provider gateway the posture
// classifier and the summarization providers use) plus a default
// model id, builds the claim-extraction prompt, calls Chat with
// retry, and parses the JSON array the model returns into
// []SentenceClaims. The provider records token usage into
// okt_system.ai_usage via the ai.Attribution the worker passes
// through.
type AIClaimExtractor struct {
	aiProvider ai.AIProvider
	model      string
	// thinkingLevel is the reasoning_effort to pass to the model
	// ("low", "medium", "high"). Empty = omit from the request.
	// Threaded into ChatRequest's ThinkingLevel field. Set to
	// "low" by the worker from ClaimExtractorConfig.ThinkingLevel.
	thinkingLevel string
}

// NewAIClaimExtractor constructs the extractor. aiProvider may be
// nil (Configured() returns false); model may be empty (Configured()
// returns false). The worker checks Configured() before calling
// Extract so a deployment without a chat provider skips the
// claim-driven retrieval path without a panic.
func NewAIClaimExtractor(aiProvider ai.AIProvider, model string) *AIClaimExtractor {
	return &AIClaimExtractor{aiProvider: aiProvider, model: model}
}

// WithThinkingLevel returns a copy of the extractor that passes the
// given reasoning_effort to the model on every Chat call. Empty
// string = omit from the request (use the model's default).
func (e *AIClaimExtractor) WithThinkingLevel(level string) *AIClaimExtractor {
	clone := *e
	clone.thinkingLevel = level
	return &clone
}

// Configured reports whether the extractor is ready to run: a non-nil
// AIProvider whose Describe().Configured is true and a non-empty model.
func (e *AIClaimExtractor) Configured() bool {
	if e == nil || e.aiProvider == nil || e.model == "" {
		return false
	}
	return e.aiProvider.Describe().Configured
}

func (e *AIClaimExtractor) Describe() ProviderDescription {
	name, configured := "(none)", false
	if e != nil && e.aiProvider != nil {
		desc := e.aiProvider.Describe()
		name = desc.Name
		configured = desc.Configured && e.model != ""
	}
	return ProviderDescription{
		Name:        "AI report claim extractor",
		Description: "Reads each report sentence and emits the verifiable assertions it makes (numeric values, causal claims, comparisons, quotations, definitions) so the annotate_report worker can retrieve facts against the specific claims rather than the sentence's broad topic.",
		Requires:    "providers.reports.claim_extractor.{enabled,provider,model} and the underlying AI provider's API key",
		Configured:  configured,
		Supports:    []string{"report_annotation"},
		Notes:       "Provider is " + name + ". Per-batch failures are logged and the batch falls back to embedding-only retrieval; the report still annotates.",
		Config: map[string]string{
			"ai_provider": name,
			"model":        e.model,
		},
	}
}

// Extract builds the prompt from req.Sentences, calls the AI
// provider's Chat, and parses the JSON array the model returns. The
// db arg is passed to the AI provider so it can record token usage
// into ai_usage (mirrors every other AI-backed provider); pass nil
// to skip recording (tests).
//
// The response is expected to be a JSON array of objects
//   [{"sentence_index": int, "claims": [{"type": "<type>", "term": "<str>", "context": "<str>"}, ...]}, ...].
// Unparseable entries are dropped (logged, not fatal) so one bad row
// doesn't invalidate the whole batch. Sentences the model omits are
// treated as having no claims (the worker uses embedding-only
// retrieval for them, which is the legacy behavior).
func (e *AIClaimExtractor) Extract(ctx context.Context, db store.DBTX, req ExtractRequest) ([]SentenceClaims, error) {
	if e.aiProvider == nil {
		return nil, fmt.Errorf("claims: ai provider not configured")
	}
	if len(req.Sentences) == 0 {
		return nil, nil
	}

	model := req.Model
	if model == "" {
		model = e.model
	}

	userMsg := buildUserMessage(req.Sentences)

	var taskID *string
	if req.TaskID != "" {
		taskID = &req.TaskID
	}

	chatReq := ai.ChatRequest{
		Model:    model,
		Messages: []ai.ChatMessage{{Role: "system", Content: claimExtractionSystemPrompt}, {Role: "user", Content: userMsg}},
		TaskID:   taskID,
		Attribution: ai.Attribution{
			RepositoryID: req.Attribution.RepositoryID,
			SourceID:     req.Attribution.SourceID,
			Operation:    "claim_extraction",
		},
	}
	if e.thinkingLevel != "" {
		tl := e.thinkingLevel
		chatReq.ThinkingLevel = &tl
	}
	if req.MaxTokens > 0 {
		mt := req.MaxTokens
		chatReq.MaxTokens = &mt
	}

	resp, err := retryWithBackoff(ctx, retryConfig{}, "claim_extract",
		func(callCtx context.Context) (ai.ChatResponse, error) {
			return e.aiProvider.Chat(callCtx, db, chatReq)
		},
	)
	if err != nil {
		return nil, fmt.Errorf("claims: ai chat failed: %w", err)
	}
	if len(resp.Messages) == 0 {
		return nil, nil
	}
	content := strings.TrimSpace(resp.Messages[0].Content)
	if content == "" {
		return nil, nil
	}
	content = trimCodeFences(content)

	parsed, err := parseClaims(content, req.Sentences)
	if err != nil {
		// Don't fail the whole report on a malformed response; the
		// caller logs and falls back to embedding-only retrieval for
		// every sentence in this batch.
		return nil, fmt.Errorf("claims: parsing extractor response: %w", err)
	}
	return parsed, nil
}

// claimExtractionSystemPrompt is the instruction prompt for the
// claim extractor. It is intentionally short and philosophy-neutral
// (the worker reads no promptset for this phase; claim extraction is
// a structural NLP pass, not a philosophy-templated generation task).
const claimExtractionSystemPrompt = `You are a claim extractor for a knowledge-graph annotation system.

For each sentence in the user message you must extract every VERIFIABLE ASSERTION the sentence makes — claims a reader could check against an external source. The goal is to drive fact retrieval against the SPECIFIC assertion, not the broad topic, so the downstream citation classifier can drop topically-adjacent facts that don't verify the sentence's actual claim.

For each sentence emit one JSON object:
  {"sentence_index": <int>, "claims": [{"type": "<type>", "term": "<verbatim surface form>", "context": "<short clause>"}]}

Claim types and what counts:
- "numeric": the sentence quotes a specific numeric value (percentage, effect size, p-value, kcal, kg, OR, RR, E-value, κ, ratio, count, threshold). The term MUST be the verbatim value + unit as it appears in the sentence (e.g. "0.9 kg", "508 kcal/day", "RR 1.15", "p = 0.04"). One claim per distinct value.
- "causal": the sentence asserts a cause-and-effect relationship (e.g. "X causes Y", "X increases Y", "X reduces Y"). The term is the short phrase naming the relationship; the context is the clause containing it.
- "comparison": the sentence compares two things (X > Y, X is higher than Y, X is opposite to Y). The term names the entities being compared; the context is the comparison clause.
- "quotation": the sentence directly quotes or paraphrases a source's finding. The term is the quoted phrase; the context is the attribution (who said it / what was found).
- "definition": the sentence defines or names a term (e.g. "NOVA classifies foods into four groups"). The term is the term being defined; the context is the definition.
- "other": a verifiable assertion that doesn't fit the above (e.g. a date, a study design, a methodology choice). Use sparingly.

Rules:
- The term must be a VERBATIM substring of the sentence (or a trivial normalization like whitespace trimming). Do NOT paraphrase — the downstream retrieval uses the term as a search key against fact text.
- Skip purely stylistic, narrative, or opinion sentences (e.g. "We now turn to...", "This section argues that...", "It is worth noting that..."). These make no verifiable claim.
- Skip meta-commentary about the report itself (e.g. "The next section discusses X").
- A sentence may have 0 claims (most prose sentences), 1 claim (a single value claim), or several (a compound sentence with multiple values/effects).
- When a sentence has 0 claims, OMIT it from the output array entirely.

Output ONLY the JSON array, COMPACT (no newlines, no indentation — emit ` + "`" + `[{"sentence_index":0,"claims":[{"type":"numeric","term":"0.9 kg","context":"0.9 kg weight gain"}]}]` + "`" + ` not pretty-printed). No prose, no headings, no explanations. Every sentence that has at least one claim must appear exactly once.`

// buildUserMessage renders the batch as a compact JSON array of
// {sentence_index, sentence} so the model has the inputs in one
// block and can emit a matching JSON array back.
func buildUserMessage(sentences []SentenceInput) string {
	type sent struct {
		SentenceIndex int    `json:"sentence_index"`
		Sentence      string `json:"sentence"`
	}
	out := make([]sent, 0, len(sentences))
	for _, s := range sentences {
		out = append(out, sent{SentenceIndex: s.Index, Sentence: s.Text})
	}
	b, _ := json.Marshal(out)
	return fmt.Sprintf(`Extract the verifiable claims from each sentence below. Output ONLY the JSON array.

%s`, string(b))
}

// parseClaims decodes the model's JSON array response into
// []SentenceClaims. It tolerates a leading/trailing code fence
// (already stripped by the caller), a single trailing comma, AND
// extra trailing junk after the closing `]` (the DeepSeek model
// occasionally emits `[]]` — an empty array followed by a stray
// bracket — which the json decoder rejects). To handle the latter,
// we find the first `[` and the matching closing `]` (counting
// bracket nesting) and decode only that span; anything after is
// ignored. Entries with a bad type, a missing/empty term, or a
// sentence_index not in the input batch are dropped (logged). The
// function returns no error for a syntactically valid but empty
// array — the caller treats that as "no claims for any sentence".
func parseClaims(content string, input []SentenceInput) ([]SentenceClaims, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, nil
	}
	// Find the first '[' and the matching closing ']' (counting
	// bracket nesting). This isolates the JSON array from any
	// trailing junk the model may have appended (e.g. `[]]`).
	start := strings.IndexByte(content, '[')
	if start < 0 {
		return nil, fmt.Errorf("response has no JSON array: %q", truncate(content, 80))
	}
	depth := 0
	end := -1
	inStr := false
	escape := false
	for i := start; i < len(content); i++ {
		c := content[i]
		if inStr {
			if escape {
				escape = false
				continue
			}
			if c == '\\' {
				escape = true
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			continue
		}
		if c == '[' {
			depth++
		} else if c == ']' {
			depth--
			if depth == 0 {
				end = i
				break
			}
		}
	}
	if end < 0 {
		// No matching close — the JSON was truncated mid-emission
		// (the model hit the max_tokens cap). Try to recover by
		// closing any open string/object/array before giving up.
		content = closeTruncatedJSON(content)
	} else {
		content = content[start : end+1]
	}
	// Tolerate a single trailing comma INSIDE the array (just
	// before the closing ]) — the model sometimes emits
	// `[...,...,]` which is invalid JSON. Strip the comma so the
	// decoder accepts it.
	content = strings.TrimSpace(content)
	if strings.HasSuffix(content, ",]") {
		content = strings.TrimSuffix(content, ",]")
		content += "]"
	}
	// Tolerate a single trailing comma AFTER the closing ] (the
	// model emitted `[...,],`).
	content = strings.TrimRight(strings.TrimSpace(content), ",")
	if content == "" {
		return nil, nil
	}

	var rows []struct {
		SentenceIndex int     `json:"sentence_index"`
		Claims        []struct {
			Type    string `json:"type"`
			Term    string `json:"term"`
			Context string `json:"context"`
		} `json:"claims"`
	}
	if err := json.Unmarshal([]byte(content), &rows); err != nil {
		// Tolerate a TRUNCATED response: the model hit the
		// max_tokens cap mid-JSON and emitted a partial array
		// (e.g. `[{...},{...` with no closing). Try to recover
		// by closing any open strings, objects, and the array,
		// then retry the decode. The recovered rows are
		// partial (the last claim/sentence may be missing
		// fields) but the json decoder will drop incomplete
		// entries via the field-omission logic below.
		recovered := closeTruncatedJSON(content)
		if recovered != content {
			if err2 := json.Unmarshal([]byte(recovered), &rows); err2 == nil {
				log.Printf("claims: recovered %d rows from truncated JSON response (original was cut mid-array)", len(rows))
			} else {
				return nil, fmt.Errorf("decoding JSON array: %w (content=%q)", err, truncate(content, 120))
			}
		} else {
			return nil, fmt.Errorf("decoding JSON array: %w (content=%q)", err, truncate(content, 120))
		}
	}

	// Build a set of valid sentence indices so we can drop rows the
	// model hallucinated.
	valid := make(map[int]bool, len(input))
	for _, s := range input {
		valid[s.Index] = true
	}

	out := make([]SentenceClaims, 0, len(rows))
	for i, r := range rows {
		if !valid[r.SentenceIndex] {
			log.Printf("claims: dropping row %d with sentence_index=%d not in input batch", i, r.SentenceIndex)
			continue
		}
		sc := SentenceClaims{SentenceIndex: r.SentenceIndex}
		for j, c := range r.Claims {
			ct := normalizeType(c.Type)
			if ct == "" {
				log.Printf("claims: dropping row %d claim %d with unknown type %q", i, j, c.Type)
				continue
			}
			term := strings.TrimSpace(c.Term)
			if term == "" {
				log.Printf("claims: dropping row %d claim %d with empty term", i, j)
				continue
			}
			sc.Claims = append(sc.Claims, Claim{Type: ct, Term: term, Context: strings.TrimSpace(c.Context)})
		}
		if len(sc.Claims) > 0 {
			out = append(out, sc)
		}
	}
	return out, nil
}

// normalizeType maps the model's type string to the canonical
// ClaimType value, accepting minor variants. Returns "" for unknown.
func normalizeType(s string) ClaimType {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "numeric", "number", "value":
		return ClaimNumeric
	case "causal", "cause", "effect":
		return ClaimCausal
	case "comparison", "compare":
		return ClaimComparison
	case "quotation", "quote":
		return ClaimQuotation
	case "definition", "define":
		return ClaimDefinition
	case "other":
		return ClaimOther
	default:
		return ""
	}
}

// trimCodeFences strips a leading ``` fence the model may add around
// its JSON despite the prompt asking for raw JSON. Mirrors the
// posture provider's trimCodeFences helper.
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

// closeTruncatedJSON attempts to repair a JSON array that was
// truncated mid-emission (the model hit the max_tokens cap before
// finishing the response). It finds the last complete top-level
// object in the array (the last `}` at depth 1) and truncates
// there, then closes the array. Returns the original string
// unchanged when the input appears balanced (no repair needed) or
// when no complete object was found (the caller falls back to the
// original decode error).
func closeTruncatedJSON(s string) string {
	// First check if the input is already balanced — if so, leave
	// it alone.
	depthArr, depthObj := 0, 0
	inStr := false
	escape := false
	lastCompleteObjEnd := -1
	for i, c := range s {
		if inStr {
			if escape {
				escape = false
				continue
			}
			if c == '\\' {
				escape = true
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			continue
		}
		if c == '[' {
			depthArr++
		} else if c == ']' {
			depthArr--
		} else if c == '{' {
			depthObj++
		} else if c == '}' {
			depthObj--
			// A `}` at array-depth 1 and object-depth 0 closes
			// a top-level object of the array.
			if depthArr == 1 && depthObj == 0 {
				lastCompleteObjEnd = i
			}
		}
	}
	if depthArr == 0 && depthObj == 0 && !inStr {
		return s
	}
	// Drop a trailing comma if the truncation happened right after
	// a comma (common when the model was cut between elements).
	s = strings.TrimRight(strings.TrimSpace(s), ",")
	// If we found at least one complete top-level object, cut
	// right after it and close the array. This drops the
	// incomplete trailing object but keeps every complete one.
	if lastCompleteObjEnd >= 0 {
		cut := s[:lastCompleteObjEnd+1]
		// Strip any trailing comma before the closing ].
		cut = strings.TrimRight(strings.TrimSpace(cut), ",")
		return cut + "]"
	}
	// No complete object found — the very first object was
	// truncated. Return the original so the caller reports the
	// real error; we can't safely recover.
	return s
}

// retryConfig + retryWithBackoff mirror the posture provider so the
// extractor rides out the same transient failures (429, 5xx,
// network) the other AI-backed providers do. Duplicated here to
// keep the claims package self-contained (no import of posture
// internals).
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
	// PerCallTO is 240s to give the non-turbo DeepSeek V4 Flash
	// model room to finish dense claim-extraction batches. The
	// previous 180s was too tight — the outer callCtx cancelled
	// mid-response, classifying as permanent (no retry). The
	// HTTP client timeout is 180s, so when the model is genuinely
	// slow (not just network-slow), the HTTP client fires first
	// and the retry classifies it as transient; when the outer
	// context fires first (this 240s), the call has been running
	// >240s and permanent is the right call.
	PerCallTO: 240 * time.Second,
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
	// context.Canceled is permanent — the caller deliberately
	// cancelled the work (e.g. the worker is shutting down).
	if errors.Is(err, context.Canceled) {
		return false, "context cancelled"
	}
	// context.DeadlineExceeded is treated as transient. The Go
	// stdlib's http.Client wraps its Timeout as a
	// context.DeadlineExceeded, and so does the outer callCtx from
	// retryWithBackoff's PerCallTO. We can't reliably distinguish
	// them by error message wording alone (OpenRouter emits both
	// "context deadline exceeded (Client.Timeout or context
	// cancellation while reading body)" and bare "context deadline
	// exceeded"). Treating both as transient is safe because the
	// retry loop checks ctx.Err() at the top of each attempt —
	// when the OUTER context is dead, the next attempt returns
	// ctx.Err() immediately without calling the LLM, so we don't
	// waste a call. When the HTTP client fired (transient), the
	// retry re-issues the call and may succeed.
	if errors.Is(err, context.DeadlineExceeded) {
		return true, "context deadline (transient — retry loop checks outer ctx)"
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