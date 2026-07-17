# Structure-Aware Chunking Provider

## Problem

`SimpleChunkingProvider` (`backend/internal/providers/decomposition/chunking.go:54`) splits parsed text on raw rune counts with a fixed overlap. Two pathologies:

1. **Mid-sentence splits** — chunks end mid-sentence / mid-thought, which the fact-extraction prompt (`fact_extraction.go:56-70`) says is exactly the case that produces fragments and unresolvable pronouns, so the model discards them.
2. **Structured knowledge shredded** — algorithms, lists, code, and procedures are split across chunks, so the "preserve the full structure" rule (`fact_extraction.go:40-42`) is impossible to satisfy.

The `chunk_index` stored on `fact_sources` (`backend/db/migrations/0013_facts.up.sql:59`) is the only artifact on the source side that depends on chunker choice, and it remains backward-compatible — old and new chunks are both 0-indexed, dense, integer streams.

## Goal

Add a **structure-aware chunker** that operates on `parsed_text` using paragraph / heading / list / code-block heuristics, register it as a second `ChunkingProvider` (id `structure_aware`), wire it as an opt-in via `providers.decomposition.chunking.provider`, and **leave existing extracted facts untouched** (the chunker only changes for new `source_decomposition` runs).

Also design the interface to take richer input later, so a future HTML-aware chunker can be added without re-plumbing the worker.

## Locked decisions

| # | Decision | Value |
|---|----------|-------|
| D1 | Strategy | **Add a new chunker** (`structure_aware`); keep `simple` for the conservative path. No changes to the existing `SimpleChunkingProvider` or its config keys. |
| D2 | Source of structure | **Use `parsed_text` only** with paragraph/heading heuristics. Do not plumb `parsed_html` through the worker in v1. The interface extension in D3 keeps the door open for a future HTML-aware provider. |
| D3 | Interface design | Extend `chunking.go` with an **optional** `StructuralChunkingProvider` interface that takes a `ChunkInput{Text, HTML}`. The worker type-asserts against it and falls back to `Chunk(text)`. `SimpleChunkingProvider` keeps its current signature; `StructureAwareChunkingProvider` implements both. |
| D4 | Backward compatibility | **Existing facts untouched.** The new chunker applies only to sources that go through `source_decomposition` from now on. No backfill, no re-extraction. |
| D5 | Default provider in `config.default.yaml` | **Opt-in.** Default stays `simple` so the upgrade story is explicit. Easy to flip later. |
| D6 | `min_chunk_size` semantics | **Informational only in v1.** Documented in `Describe()`; small trailing chunks are emitted as-is, not merged into the previous one. (Can be made a hard floor in a 5-line follow-up.) |
| D7 | Dependencies | **stdlib only** (`unicode`, `regexp`, `strings`). No new third-party packages. |

## Algorithm: `StructureAwareChunkingProvider`

New file: `backend/internal/providers/decomposition/structure_aware.go`.

1. **Block tokenisation** on `parsed_text`:
   - Split on `\n\n+` (one or more blank lines) to get coarse blocks. Mirrors the parser's whitespace normalization in `content_parsing/content_parsing.go:67-68` (single newlines within block, blank lines between).
   - For each block, classify into one of four kinds by lightweight regex heuristics:
     - `heading` — short line (<120 runes), no terminal punctuation, looks title-cased. Used for *grouping only* — it is never emitted as a chunk on its own, mirroring the fact-extraction rule that headings alone are not facts.
     - `list` — every non-blank line in the block starts with `- `, `* `, `1. `, `(1)`, etc.
     - `code` — high symbol density, short lines without terminal punctuation, or leading whitespace indentation. Heuristic only; we are not parsing markdown.
     - `paragraph` — everything else.
2. **Grouping into chunks** (rune budget = `chunk_size`):
   - Greedy: accumulate `paragraph` / `list` / `code` blocks into a chunk until the next block would push it over `chunk_size`.
   - `heading` blocks are not consumed as their own chunk; they attach to the *next* non-heading block. This keeps the section context attached to the facts the model extracts.
   - `list` and `code` blocks are **atomic** — never split, even if the block alone exceeds `chunk_size`. If a single block is larger than `chunk_size`, fall back to:
     1. Sentence-aware split inside the block (split on `[.!?] + `, never inside a word).
     2. Hard split on the last whitespace within a window.
3. **Overlap**: take the trailing *blocks* of the previous chunk whose combined rune count ≤ `chunk_overlap` and prepend them to the next chunk. Block-boundary overlap, not raw rune overlap — keeps the overlap window coherent.
4. **Edge cases** (mirror `SimpleChunkingProvider`):
   - empty input → `nil`.
   - input smaller than `chunk_size` → single chunk.
   - `chunk_overlap >= chunk_size` → clamp to `chunk_size / 10` (same convention as `NewSimpleChunkingProvider:45-47`).
5. **Index assignment** — same dense, 0-based stream as the simple chunker, so `fact_sources.chunk_index` semantics are preserved.

## Interface design (future-proofing)

Extend `backend/internal/providers/decomposition/chunking.go`:

```go
// StructuralChunkingProvider is implemented by chunkers that
// can use the source's structured fields (parsed HTML) when
// available. The worker type-asserts the registered chunker
// against this interface; chunkers that don't implement it
// continue to receive plain text via Chunk() and ignore the
// HTML field.
type StructuralChunkingProvider interface {
    ChunkStructured(input ChunkInput) []Chunk
}

type ChunkInput struct {
    Text string // always set
    HTML string // may be empty; "" means the chunker should fall back to Text
}
```

Worker change in `backend/internal/taskmanager/tasks/source_decomposition.go:113`:

```go
var chunks []decomposition.Chunk
if scp, ok := w.chunkingProvider.(decomposition.StructuralChunkingProvider); ok {
    chunks = scp.ChunkStructured(decomposition.ChunkInput{
        Text: text,
        HTML: deref(source.ParsedHTML),
    })
} else {
    chunks = w.chunkingProvider.Chunk(text)
}
```

`SimpleChunkingProvider` keeps its `Chunk(text)` method as-is — it doesn't implement `StructuralChunkingProvider`, so the simple path is unchanged. The new `StructureAwareChunkingProvider` implements both, and v2 (HTML-aware) can extend `ChunkStructured` without touching the worker again.

## Configuration

Extend `DecompositionChunkingConfig` in `backend/internal/config/config.go:215-219`:

```go
type DecompositionChunkingConfig struct {
    Provider        string `mapstructure:"provider"`
    ChunkSize       int    `mapstructure:"chunk_size"`
    ChunkOverlap    int    `mapstructure:"chunk_overlap"`
    MinChunkSize    int    `mapstructure:"min_chunk_size"`    // default 0 = no floor
    RespectLists    *bool  `mapstructure:"respect_lists"`     // default true
    RespectCode     *bool  `mapstructure:"respect_code"`      // default true
    KeepHeadings    *bool  `mapstructure:"keep_headings"`     // default true
}
```

`backend/configs/config.default.yaml:130-132` updates:

```yaml
chunking:
  # provider: "structure_aware"  # uncomment to opt in
  provider: "simple"
  chunk_size: 2000
  chunk_overlap: 200
  min_chunk_size: 0
  respect_lists: true
  respect_code: true
  keep_headings: true
```

Pointer-to-bool so the absence of the key in `config.local.yaml` means "use provider default" (true), not "false".

## Wiring

`backend/cmd/app/api.go:148-153` — build both providers, register both:

```go
chunkingProviders := map[string]decomposition.ChunkingProvider{
    "simple": decomposition.NewSimpleChunkingProvider(
        cfg.Providers.Decomposition.Chunking.ChunkSize,
        cfg.Providers.Decomposition.Chunking.ChunkOverlap,
    ),
    "structure_aware": decomposition.NewStructureAwareChunkingProvider(
        cfg.Providers.Decomposition.Chunking,
    ),
}
```

The taskmanager's existing provider-selection block (`backend/internal/taskmanager/taskmanager.go:147-156`) already supports a config-driven provider id and falls back to `simple` when unset — no change needed there.

`Describe()` on the new provider surfaces its effective config (including the new keys) in `Config`, so the `/sources/decomposition/providers` endpoint and the Providers UI card show what the chunker is actually doing.

## Tests

### Unit (new file: `backend/internal/providers/decomposition/structure_aware_test.go`)

| Test | Asserts |
|---|---|
| `TestStructureAware_EmptyInput` | nil chunks |
| `TestStructureAware_ShorterThanChunkSize` | single chunk, text == input |
| `TestStructureAware_ParagraphsNotSplit` | three paragraphs each 500 runes with `chunk_size=1500` → 1 chunk |
| `TestStructureAware_LargeParagraphSplitOnSentence` | 1 paragraph of 4000 runes → multiple chunks, none end mid-word |
| `TestStructureAware_CodeBlockAtomic` | fenced code block of 3000 runes with `chunk_size=1000` → 1 chunk (atomic) |
| `TestStructureAware_ListAtomic` | 20-item list totaling 3000 runes → 1 chunk |
| `TestStructureAware_HeadingPrependedToNext` | heading alone → not emitted; heading + paragraph → heading at top of chunk |
| `TestStructureAware_OverlapRespectsBlockBoundaries` | overlap is taken from complete trailing blocks, not partial runes |
| `TestStructureAware_ImplementsStructuralInterface` | `*StructureAwareChunkingProvider` satisfies `StructuralChunkingProvider` |
| `TestStructureAware_ConfigDescribe` | `Describe().Config` includes all 5 new keys with effective values |
| `TestStructureAware_DefaultOverrides` | `NewStructureAwareChunkingProvider` clamps `chunk_overlap` to `chunk_size/10` when too large |

### E2E (extend `backend/e2e/decomposition_providers_test.go`)

- `TestDecompositionProvidersEndpointWithStructureAware` — mirror the existing `TestDecompositionProvidersEndpointWithChunker` (`decomposition_providers_test.go:98`) but use a new testutil helper that wires `structure_aware` as the chunker. Assert id, name, `supports=["chunking"]`, `configured=true`, and the 5 config keys are present.

- `TestStructureAwareChunkingBoundaries` — set parsed text directly on a source row (via the testutil fixture, similar to how `source_parsed_test.go` does it) with three labelled paragraphs, enqueue `source_decomposition`, wait for the worker, and read the persisted `fact_sources.chunk_index` rows + their `fact.text` to assert no fact crosses a paragraph boundary. Skips gracefully when no fact-extraction AI provider is configured (same skip pattern as the SERPER_API_KEY-gated test in the testing policy).

The `testutil` package gets one new constructor, `NewTestEnvWithStructureAwareChunker(t, chunkingCfg)`, mirroring the existing `NewTestEnvWithChunker` pattern.

## Files changed / created

| File | Change |
|---|---|
| `backend/internal/providers/decomposition/structure_aware.go` | **new** — provider implementation |
| `backend/internal/providers/decomposition/structure_aware_test.go` | **new** — unit tests |
| `backend/internal/providers/decomposition/chunking.go` | add `ChunkInput` + `StructuralChunkingProvider` interface |
| `backend/internal/config/config.go:215-219` | extend `DecompositionChunkingConfig` |
| `backend/configs/config.default.yaml:130-132` | document + default the new keys |
| `backend/cmd/app/api.go:148-153` | register `structure_aware` |
| `backend/internal/taskmanager/tasks/source_decomposition.go:113` | type-assert against `StructuralChunkingProvider`; pass `ParsedHTML` |
| `backend/e2e/decomposition_providers_test.go` | add 2 e2e tests |
| `backend/e2e/testutil/` | add `NewTestEnvWithStructureAwareChunker` |

## Out of scope (and why)

- **No re-extraction of existing sources.** Per D4, the new chunker only applies to sources that go through `source_decomposition` from now on.
- **No migration of the `chunk_index` semantics.** Old and new chunk streams are both 0-indexed dense integers; `fact_sources.chunk_index` is opaque to the UI other than for display.
- **No LLM cost.** Pure Go, paragraph + heading + list + code heuristics. The "semantic" awareness is structural, not topic-based.
- **No changes to `fact_extraction` or the prompt.** The chunker is upstream of the prompt; better chunk boundaries translate directly to higher-quality extraction without retuning the prompt.
- **No new third-party dependencies.** Adds ~200-300 lines of Go with stdlib only.

## Open questions

1. Default provider in `config.default.yaml` — resolved as opt-in (D5). Easy to flip later.
2. `min_chunk_size` semantics — resolved as informational only (D6). Can be made a hard floor in a 5-line follow-up if desired.
