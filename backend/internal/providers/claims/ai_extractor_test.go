package claims

import (
	"context"
	"errors"
	"testing"
)

func TestParseClaims_ToleratesExtraTrailingBracket(t *testing.T) {
	// The DeepSeek model occasionally emits "[]]" — an empty array
	// followed by a stray bracket. parseClaims must isolate the
	// first [...] span and ignore the trailing junk.
	input := []SentenceInput{{Index: 0, Text: "Hall 2019 reported 0.9 kg weight gain."}}
	got, err := parseClaims("[]]", input)
	if err != nil {
		t.Fatalf("parseClaims([]]): unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("parseClaims([]]): expected 0 sentences, got %d", len(got))
	}
}

func TestParseClaims_ToleratesTrailingJunkAfterArray(t *testing.T) {
	// A non-empty array followed by stray junk. The parser must
	// decode the array and ignore the trailing "extra]".
	input := []SentenceInput{{Index: 0, Text: "The RCT produced 0.9 kg weight gain."}}
	content := `[{"sentence_index":0,"claims":[{"type":"numeric","term":"0.9 kg","context":"0.9 kg weight gain"}]}]extra]`
	got, err := parseClaims(content, input)
	if err != nil {
		t.Fatalf("parseClaims: unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 sentence, got %d", len(got))
	}
	if got[0].SentenceIndex != 0 {
		t.Errorf("SentenceIndex = %d, want 0", got[0].SentenceIndex)
	}
	if len(got[0].Claims) != 1 {
		t.Fatalf("expected 1 claim, got %d", len(got[0].Claims))
	}
	if got[0].Claims[0].Type != ClaimNumeric {
		t.Errorf("claim type = %q, want %q", got[0].Claims[0].Type, ClaimNumeric)
	}
	if got[0].Claims[0].Term != "0.9 kg" {
		t.Errorf("claim term = %q, want %q", got[0].Claims[0].Term, "0.9 kg")
	}
}

func TestParseClaims_ToleratesTrailingComma(t *testing.T) {
	input := []SentenceInput{{Index: 0, Text: "The RCT produced 0.9 kg weight gain."}}
	content := `[{"sentence_index":0,"claims":[{"type":"numeric","term":"0.9 kg","context":""}]},]`
	got, err := parseClaims(content, input)
	if err != nil {
		t.Fatalf("parseClaims: unexpected error: %v", err)
	}
	if len(got) != 1 || len(got[0].Claims) != 1 {
		t.Fatalf("expected 1 sentence with 1 claim, got %v", got)
	}
}

func TestParseClaims_DropsHallucinatedSentenceIndex(t *testing.T) {
	input := []SentenceInput{{Index: 0, Text: "Sentence 0."}}
	// The model emitted a row for sentence_index=42 which was NOT
	// in the input batch. parseClaims must drop it.
	content := `[{"sentence_index":42,"claims":[{"type":"numeric","term":"0.9 kg","context":""}]}]`
	got, err := parseClaims(content, input)
	if err != nil {
		t.Fatalf("parseClaims: unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 sentences (hallucinated index dropped), got %d", len(got))
	}
}

func TestParseClaims_DropsBadTypeAndEmptyTerm(t *testing.T) {
	input := []SentenceInput{{Index: 0, Text: "Sentence 0."}}
	content := `[{"sentence_index":0,"claims":[{"type":"bogus","term":"x","context":""},{"type":"numeric","term":"","context":""},{"type":"numeric","term":"0.9 kg","context":""}]}]`
	got, err := parseClaims(content, input)
	if err != nil {
		t.Fatalf("parseClaims: unexpected error: %v", err)
	}
	if len(got) != 1 || len(got[0].Claims) != 1 {
		t.Fatalf("expected 1 sentence with 1 surviving claim, got %v", got)
	}
	if got[0].Claims[0].Term != "0.9 kg" {
		t.Errorf("surviving claim term = %q, want %q", got[0].Claims[0].Term, "0.9 kg")
	}
}

func TestClassifyError_HTTPClientTimeoutIsRetryable(t *testing.T) {
	// The OpenRouter provider's http.Client.Timeout fires as a
	// context.DeadlineExceeded wrapped in a "Client.Timeout or
	// context cancellation while reading body" message. This must
	// be classified as retryable so a slow upstream doesn't kill
	// the whole batch.
	err := errors.New("openrouter: decoding response: context deadline exceeded (Client.Timeout or context cancellation while reading body)")
	retryable, reason := classifyError(err)
	if !retryable {
		t.Errorf("expected HTTP client timeout to be retryable, got permanent (reason=%q)", reason)
	}
}

func TestClassifyError_OuterContextDeadlineIsRetryable(t *testing.T) {
	// context.DeadlineExceeded is treated as transient — the retry
	// loop checks ctx.Err() at the top of each attempt, so retrying
	// a dead outer context is safe (the next attempt returns
	// immediately without calling the LLM).
	err := context.DeadlineExceeded
	retryable, _ := classifyError(err)
	if !retryable {
		t.Errorf("expected context.DeadlineExceeded to be retryable, got permanent")
	}
}

func TestClassifyError_ContextCanceledIsPermanent(t *testing.T) {
	err := context.Canceled
	retryable, _ := classifyError(err)
	if retryable {
		t.Errorf("expected context.Canceled to be permanent, got retryable")
	}
}

func TestParseClaims_RecoversTruncatedJSON(t *testing.T) {
	// The model hit the max_tokens cap mid-emission and produced
	// a truncated array. parseClaims must recover by cutting at
	// the last complete top-level object and closing the array.
	// Here the first sentence (80) is complete and the second
	// (81) is truncated mid-object, so recovery should keep
	// sentence 80 and drop 81.
	input := []SentenceInput{{Index: 80, Text: "Sentence 80."}, {Index: 81, Text: "Sentence 81."}}
	truncated := `[
  {"sentence_index":80,"claims":[{"type":"numeric","term":"80%","context":"F1 ceiling"}]},
  {"sentence_index":81,"claims":[{"type":"numeric","term":"trun`
	got, err := parseClaims(truncated, input)
	if err != nil {
		t.Fatalf("parseClaims: unexpected error on truncated JSON: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 recovered sentence (80), got %d: %v", len(got), got)
	}
	if got[0].SentenceIndex != 80 {
		t.Errorf("recovered sentence_index = %d, want 80", got[0].SentenceIndex)
	}
	if len(got[0].Claims) != 1 {
		t.Fatalf("expected 1 claim for sentence 80, got %d", len(got[0].Claims))
	}
	if got[0].Claims[0].Term != "80%" {
		t.Errorf("recovered claim term = %q, want %q", got[0].Claims[0].Term, "80%")
	}
}

func TestCloseTruncatedJSON_CutsAtLastCompleteObject(t *testing.T) {
	// Two top-level objects: the first complete, the second
	// truncated. Recovery should keep the first and close the array.
	in := `[{"a":1},{"a":2,"b":`
	out := closeTruncatedJSON(in)
	want := `[{"a":1}]`
	if out != want {
		t.Errorf("closeTruncatedJSON = %q, want %q", out, want)
	}
}

func TestCloseTruncatedJSON_LeavesBalancedInputUnchanged(t *testing.T) {
	in := `[{"a":"b"}]`
	if out := closeTruncatedJSON(in); out != in {
		t.Errorf("closeTruncatedJSON on balanced input = %q, want %q", out, in)
	}
}

func TestCloseTruncatedJSON_NoCompleteObjectReturnsOriginal(t *testing.T) {
	// The very first object is truncated — no complete object to
	// cut at. Returns the original so the caller reports the real
	// error.
	in := `[{"a":`
	out := closeTruncatedJSON(in)
	if out != in {
		t.Errorf("closeTruncatedJSON with no complete object = %q, want original %q", out, in)
	}
}