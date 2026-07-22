# Scope E — Implementation Report: Open Knowledge Tree (OKT)

A code-grounded implementation report for the Open Knowledge Tree at
`/home/charlie/Documents/workspace/love/open-knowledge-tree-go`. Every claim
below is cited to an exact file:line and, where relevant, the literal config
value or SQL that realises it. The report covers 12 topics and ends with a
quick-reference table mapping topic → file → key value.

---

## 1. Fact decomposition

The text-fact extraction worker lives in
`backend/internal/taskmanager/tasks/source_decomposition.go`. It is a River
worker (`SourceDecompositionWorker`, line 81) whose `Work` method (line 122)
runs the two-phase extract-then-persist loop.

**Chunking + model.** Chunk size and overlap are not hardcoded in the worker;
they are config-driven via `providers.decomposition.chunking` in
`backend/configs/config.default.yaml:356-358`:

```yaml
chunking:
  chunk_size: 2000
  chunk_overlap: 200
```

The fact-extraction model is `google/gemma-4-31b-it`
(`backend/configs/config.default.yaml:359-361`, `fact_extraction.model`), served
via OpenRouter (`provider: "openrouter"`). The chunk-level fan-out is bounded by
`fact_extraction.concurrency: 4` (config line 368); the worker reads it via
`w.factCfg.ConcurrencyOr(4)` at `source_decomposition.go:296` and uses
`decomposition.ExtractParallel` (line 297) for phase 1.

**Prompt + 9 fact types.** The prompt is `builtinFactExtractionPrompt` in
`backend/internal/promptset/builtin_prompts.go:22-169`. It enumerates nine fact
types (lines 27-44): **claim, account, measurement, formula, quote, procedure,
reference, code, perspective** — each with a description, length hint, and a
worked example. The prompt also encodes the self-containedness rules (lines
65-97), the SKIP list (lines 83-96), and the response shape
`[{"text":"...","sentences":[0,1]}]` (lines 167-169).

**Promptset hash.** The worker resolves the repo's effective promptset once at
`Work()` start (`source_decomposition.go:154-163`) and stores
`psHashPtr := &psHash` (line 164). That pointer is written into every fact row
as `PromptsetHash` (`CreateFact` call at line 336-341) and every
`fact_references` row (line 366-372), so decompositions from different
philosophies never mix.

**`buildLabeledChunkText` + `[Sn]` prefixes.** When the source has a sentence
array (persisted by the retrieve_source worker), each chunk is rewritten by
`buildLabeledChunkText` (`source_decomposition.go:782-794`). It emits only the
global sentences overlapping the chunk's rune range, each prefixed with its
global `[Sn]` index (`fmt.Fprintf(&b, "[S%d] %s ", s.Index, ...)` at line 792).
The model echoes those indices back; the worker writes them directly to
`fact_references` without any chunk-relative translation (lines 362-376).

**Sentence provenance.** `fact_references` is one row per cited global sentence
index. Hallucinated indices (out of range) are silently dropped — the fact is
still kept, just without a bad reference (`source_decomposition.go:362-365`).

**Fact statuses.** Facts carry a `status` column with three values, declared in
`backend/db/migrations/0013_facts.up.sql:43-44`:
`CHECK (status IN ('new', 'stable', 'to_delete'))`. `new` = freshly extracted;
`stable` = confirmed unique after dedup; `to_delete` = flagged as duplicate.

**`facts` table — no `source_id` column.** The `facts` table
(`backend/db/migrations/0013_facts.up.sql:40-48`) has columns `id, text, status,
embedded_at, embedded_model, created_at` and (after migration 0016) `image_url,
fact_kind`. There is **no `source_id` column** — the migration's header comment
is explicit (lines 7-12): "There is no 'origin' concept and no `facts.source_id`
column — all source links live in the junction." The N:M link is
`fact_sources` (lines 56-62) with `PRIMARY KEY (fact_id, source_id)` and a
per-extraction `chunk_index`. `AddFactSource`'s `ON CONFLICT` clause makes the
link idempotent on re-process or dedup merge (`source_decomposition.go:350-357`).

**Two-phase pattern.** Phase 1 fans chunks out to the LLM with bounded
concurrency (`source_decomposition.go:286-311`); phase 2 persists serially in
input order (lines 313-382) so the DB sees the same connection pressure as the
serial baseline. The worker chains to `embed_facts` when `totalFacts > 0`
(lines 426-445).

---

## 2. Concept extraction + contexts + aliases

The concept-extraction worker is `ExtractConceptsWorker` in
`backend/internal/taskmanager/tasks/extract_concepts.go` (`Work` at line 197).
Model is `google/gemma-4-31b-it` via OpenRouter
(`backend/configs/config.default.yaml:402-405`). Batch size and concurrency are
`fact_batch_size: 10` and `concurrency: 4` (config lines 414, 425); the worker
derives one wave's fetch size as `Concurrency × FactBatchSize`
(`extract_concepts.go:294-299`).

**`concepts` table uniqueness.** `backend/db/migrations/0023_concepts.up.sql`
defines the concepts table (lines 24-33) and a case-insensitive composite
uniqueness index (lines 42-43):

```sql
CREATE UNIQUE INDEX uq_concepts_repo_name_context
    ON okt_repository.concepts (repository_id, lower(canonical_name), lower(context));
```

So the identity of a concept is `(repository_id, lower(canonical_name),
lower(context))` — the same name with different casing is the same concept,
and the same name in different contexts is a different concept.

**`repository_contexts`.** The allowed context vocabulary is per-repo, loaded
at `Work()` start from `ListRepositoryContexts`
(`extract_concepts.go:275-285`). An empty list hard-fails (line 279-281): the
admin must configure contexts via the repository-settings UI. The backfill
migration seeds every legacy repo with the full context vocabulary. The
builtin concept-extraction prompt (`builtinConceptExtractionPrompt`,
`builtin_prompts.go:222-272`) is formatted with the L3 ontology class list
(lines 254-256) and instructs the model: "The context MUST be one of the labels
in the list above, verbatim" (line 261).

**Context vocabulary sizes.** The DBpedia L3 source list is
`scripts/experiments/dbpedia_l3.json` — a JSON list of **789** labels. The
curated reduced list is `scripts/experiments/manual_select.json`, whose
`categories` array contains **88** entries (the "general" preset's `all` is
resolved to the full ~88-category vocabulary at creation time per
`config.default.yaml:820-825`). The **scientific** repository preset
(`config.default.yaml:837-873`) lists ~30 scientific contexts (Biomolecule,
chemical compound, gene, protein, drug, disease, etc.).

**`concept_aliases`.** The aliases table
(`0023_concepts.up.sql:45-50`) carries alternate surface forms. Uniqueness is
case-insensitive per concept (lines 57-58):
`uq_concept_aliases_concept_text ON okt_repository.concept_aliases (concept_id,
lower(alias_text))`. A lookup index on `lower(alias_text)` (lines 51-52) backs
the text-search match path. Seed aliases are emitted by the
concept-extraction model (`builtinConceptExtractionPrompt` "Seed aliases"
section, `builtin_prompts.go:240-252`) and merged onto matched concepts for
free via `AddConceptAlias`'s `ON CONFLICT DO NOTHING`
(`extract_concepts.go:828-839, 946-960`).

**Per-fact disambiguation `ResolveAliasMatchForFact`.** When an alias is shared
by multiple concepts (e.g. "N" on both Nitrogen and Neutron), the helper in
`backend/internal/concepts/resolve.go:62-94` picks, for ONE specific fact, the
concept whose Qdrant vector is cosine-closest to that fact's vector. Strategy
(lines 41-61): `FindConceptsByAlias` (:many, deterministic ORDER BY) → 0 matches
returns false (miss); 1 match returns it; >1 matches runs the embedding
tie-break in `disambiguateByEmbedding` (lines 101-205).

**Cosine tie-break.** `disambiguateByEmbedding` fetches the fact's Qdrant vector
via `GetFactVectorsByIDs` (line 122), fetches candidate concept vectors via
`GetConceptVectorsByIDs` (line 141), embeds-in-place any candidate missing a
vector (lines 152-176), and cosine-compares via `cosineSim` (lines 270-285).
The in-place embedding input is `c.CanonicalName + " " + c.Context` — see
`embedConceptInPlace` line 224: `input := c.CanonicalName + " " + c.Context`.

**Embed-in-place.** When a matched concept has no Qdrant vector, it is embedded
on the spot (`embedConceptInPlace`, `resolve.go:213-248`), upserted via
`UpsertConceptVectors` (line 240), and marked embedded via `MarkConceptEmbedded`
(line 257) so the later `embed_concepts` pass skips it.

---

## 3. Alias merging / pruning

`RefineConceptsWorker` in
`backend/internal/taskmanager/tasks/refine_concepts.go` (`Work` at line 136)
resolves unresolved `concept_candidates` in a three-stage routing loop. Config
is `providers.refinement` in `config.default.yaml:633-649`:

```yaml
refinement:
  enabled: false
  model: "google/gemma-4-31b-it"
  max_candidates_per_run: 40
  prune_threshold: 5
  max_tokens: 400
  max_concurrency: 5
```

**`mergeCandidateIntoConcept`** (`refine_concepts.go:537-604`) moves the
candidate's facts onto the target concept
(`ReassignFactCandidatesToConcept`, lines 547-552), copies seed aliases (lines
555-566) and AI aliases (lines 569-580), adds the concept_text itself as an
alias (lines 584-589), resets the target's embedding
(`ResetConceptEmbedding`, line 592), and resolves the candidate (cache entry,
lines 597-599).

**`applyPruning`** (`refine_concepts.go:629-643`) deletes each alias in
`pruneList` from the concept via `DeleteConceptAliasByText`, counting
`AliasesPruned` atomically. The LLM returns the prune list as
`aliases_to_prune` in the JSON shape
`{"canonical_name":"...","aliases_to_add":[...],"aliases_to_prune":[...]}`
(see `builtinRefinementPrompt`,
`backend/internal/promptset/builtin_prompts.go:274-302`, response shape at line
300-301).

**`prune_threshold: 5`** is the re-prune gate
(`config.default.yaml:638`): an established concept's aliases are re-pruned
only when ≥ 5 new aliases have accumulated since the last refinement (comment
at config lines 631-632).

**Three-stage routing (pre-LLM → LLM → post-LLM → create).** Documented in the
`Work` doc comment (`refine_concepts.go:118-131`) and implemented in
`refineOneCandidate` (lines 282-530):

1. **Pre-LLM (all DB, no LLM)** — `findConceptByCanonical` (step 1, line 307),
   `findConceptsByAlias` with single-target merge or the per-fact
   `tryRouteAliasAmbiguous` fork when >1 match (step 2, lines 319-333), and
   seed-alias match (step 3, lines 336-362).
2. **LLM call** — only if all pre-LLM routes miss: one `refiner.Refine` call
   with a 120s background context (lines 365-384), gated by a per-canonical
   advisory lock (`TryAdvisoryLockForSynthesis`, lines 393-404) so two
   goroutines don't race on the same AI canonical.
3. **Post-LLM (all DB, reuses LLM output)** — AI canonical match (step 4, line
   414), AI alias match (step 5, lines 424-456).
4. **Create** — if all miss, `CreateConcept` (line 459) with the AI canonical
   name + AI aliases + seed aliases, then `ReassignFactCandidatesToConcept`
   (line 516) and `resolveCandidate` (line 524).

**Concurrency.** The chunk of candidates is processed in parallel via an
`errgroup` with `g.SetLimit(w.concurrency)` (line 239), default 5
(`NewRefineConceptsWorker` line 105, `cfg.MaxConcurrencyOr(5)`).

---

## 4. Fact deduplication

`DeduplicateFactsWorker` in
`backend/internal/taskmanager/tasks/deduplicate_facts.go` (`Work` at line 118).

**Embedding model + dimensions.** `providers.embedding` in
`config.default.yaml:434-437`:

```yaml
embedding:
  provider: "openrouter"
  model: "google/gemini-embedding-2"
  dimensions: 3072
```

So facts are embedded with `google/gemini-embedding-2` into 3072-dim vectors and
upserted into the shared `okt_facts` Qdrant collection
(`config.default.yaml:449-458`).

**Threshold `0.94`, cosine.** `providers.dedup` in
`config.default.yaml:459-466`:

```yaml
dedup:
  threshold: 0.94
  catchup_max_age: 168h
```

The worker reads it via `w.dedupCfg.Threshold`
(`deduplicate_facts.go:255`) and passes it as `float32(w.dedupCfg.Threshold)`
to `qdrant.SearchSimilarByID` with `limit=1`. Qdrant returns the nearest
neighbor by cosine similarity; the worker's `cosineSim` helper
(`concepts/resolve.go:270-285`) is also used by the alias tie-break in topic 2.

**Stable-wins.** The loop sorts stable-first, then new, UUID-ascending within
each group (`deduplicate_facts.go:189-200`). For each `new` fact `nf`, it
searches Qdrant for the nearest neighbor (excluding self, score ≥ threshold,
limit=1). The `switch hitStatus` (lines 272-333):
- `case "stable"`: mark `nf` `to_delete`, merge `nf`'s sources onto the stable
  survivor (lines 273-289). **Stable always wins over new.**
- `case "new"`: the current `nf` wins; the hit is marked `to_delete`
  immediately and `statusByID[hit.ID]` is mutated so the loop skips it when
  reached later (lines 290-329).
- `case "to_delete"`: skip (not a valid keeper, lines 330-332).

**New-vs-new order-dependent tie-break.** The doc comment at lines 108-117 is
explicit: the previous rule was "lex-larger UUID loses" (deterministic but
symmetric); the new rule is "the hit loses" — *order-dependent* and relies on
the stable-first, new-UUID-ascending sort (lines 189-200). The first new fact
in UUID order to find a near-duplicate twin wins; the twin is skipped when the
loop reaches it. This catches same-batch duplicates the old rule missed.

**`mergeSources`** (`deduplicate_facts.go:418-451`) is the canonical merge
routine:
- `AddFactSource` for each of the loser's sources, idempotent via the
  junction's `ON CONFLICT` clause (lines 423-431). The chunk_index from the
  loser's link is preserved.
- `DeleteDuplicateFactReferences` drops the loser's citations that would
  collide with the winner's existing citations (same source_id +
  sentence_index) (lines 432-436).
- `RelinkFactReferences` moves the remaining rows onto the winner
  (lines 437-443) — **non-overlapping citations from both facts are
  preserved** (the dedup-preserves-all-references guarantee, doc lines
  412-417).
- `RelinkFactConcepts` moves the loser's fact_concepts rows onto the winner
  (lines 444-449).

**`pg_advisory_xact_lock(hashtext(repository_id))`.** The whole dedup pass
runs inside a single transaction (`deduplicate_facts.go:149-153`). The lock is
taken on the per-repo pool (line 162):

```go
tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", args.RepositoryID)
```

It is transaction-scoped (released on commit/rollback), so two enqueues for the
same repo always serialize on their `hashtext(repository_id)` value (comment
lines 155-164). Two repos in different databases can dedup in parallel.

**`fact_catchup` 168h.** `catchup_max_age: 168h` (config line 466, = 7 days) is
the age beyond which the daily `fact_catchup` periodic job reaps stuck
`to_delete`/`new` facts (config comment lines 460-463). The worker promotes
surviving `new` facts to `stable` via `MarkFactsStableByRepo` (line 339) and
flips their Qdrant payload via `UpdateFactStatusPayload` (lines 361-365).

**Chain.** The worker chains to `extract_concepts` on completion (lines 380-394)
— the serial pipeline is `dedup → extract_concepts → embed_concepts → cleanup`
(comment lines 374-379).

---

## 5. Hierarchical summarization + synthesis

**Summarization.** `SummarizeConceptsWorker` in
`backend/internal/taskmanager/tasks/summarize_concepts.go` (`Work` at line
124). Config is `providers.summarization` in `config.default.yaml:595-620`:

```yaml
summarization:
  enabled: false
  model: "google/gemma-4-31b-it"
  batch_size: 20
  max_concepts_per_run: 40
  lock_staleness: 2h
  max_tokens: 600
```

**Batch-only mode.** Once a concept has at least one complete (frozen) summary
slice, the worker switches from the incremental open-accumulator path to
batch-only: only emit complete `BatchSize` slices, never an open remainder,
and skip the pass entirely when fewer than `BatchSize` new facts arrived
(`summarize_concepts.go:280-303`). The check is `hasComplete && len(uncovered)
< batchSize` → `PairsSkippedNoDelta++` (lines 300-303). This caps LLM cost —
the first slice absorbs the per-fact regeneration cost while a concept is
small, then the worker waits for a full batch before spending another call
(comment lines 280-289).

**Open accumulator + `is_complete`.** While no complete slice exists yet, the
worker also emits one open remainder (`is_complete=FALSE`) as the incremental
accumulator (`summarize_concepts.go:354-374`). Once a complete slice is written
(`hadComplete = true` at line 447), the loop breaks (line 369-371) so leftover
facts stay uncovered until a full batch gathers. The slice write goes through
`upsertSlice` (lines 513-557): update the existing open row when `openID.Valid
&& openSeq == seq` (lines 528-541), otherwise create a new row (lines 542-554).

**Unique partial index — one open slice per concept.**
`backend/db/migrations/0026_concept_summaries.up.sql:46-48`:

```sql
CREATE UNIQUE INDEX IF NOT EXISTS uq_concept_summaries_concept_open
    ON okt_repository.concept_summaries (concept_id)
    WHERE is_complete = FALSE;
```

The worker relies on this invariant — `GetOpenSummary` is a scalar lookup
(migration comment lines 18-21). The per-concept lock is
`concepts.summarizing_at` (migration lines 56-57), claimed via
`ClaimConceptForSummary` with a staleness window
(`summarize_concepts.go:236-250`, default 2h).

**Synthesis.** `SynthesizeConceptsWorker` in
`backend/internal/taskmanager/tasks/synthesize_concept.go` (`Work` at line
100). Config is `providers.synthesis` in `config.default.yaml:650-691`:

```yaml
synthesis:
  enabled: true
  model: "deepseek/deepseek-v4-flash:turbo"
  image_picker_model: "google/gemma-4-31b-it"
  max_tokens: 10000
  thinking_level: "low"
  max_images: 10
  max_image_candidates: 50
  max_related_concepts: 10
  max_related_syntheses: 3
```

**Group key.** The synthesis group key is `repoIDStr + ":" +
strings.ToLower(concept.CanonicalName)` (`synthesize_concept.go:214`). The
worker resolves the concept_id to its canonical-name group, loads ALL summary
slices across the group via `ListSummariesByCanonicalNameGroup` (lines 238-241)
and the group's concept_ids via `ListGroupConceptIDs` (lines 252-255).

**No-delta skip.** Before running the LLM, the worker loads the existing
synthesis via `GetSynthesisByGroup` (lines 264-267) and skips when
`coversAll(existing.CoveredSummaryIds, slices)` (lines 273-276, helper at
498-521). This prevents redundant regeneration when the inputs are unchanged.

**Related concepts top 10 + top 3 syntheses verbatim.** `loadRelatedConcepts`
(lines 584-670) loads the top `max_related_concepts` (=10) related concepts by
`shared_fact_count` from the `concept_relations` matview via
`ListConceptRelationsByConceptName` (lines 599-604). Of those, the top
`max_related_syntheses` (=3) by rank also carry their existing synthesis text
verbatim via `GetSynthesisByGroup` (lines 654-665). Per-context breakdown is
capped at `maxRelatedContexts = 5` (line 561, applied at lines 637-640).

**Image picker max 10.** Image candidates are loaded via `ListGroupImageFacts`
capped at `max_image_candidates: 50` (line 323-327). When the candidate count
exceeds `max_images: 10`, the picker LLM call runs (`PickImages`, lines 341-356)
with `MaxImages: maxImages`; otherwise all candidates pass through directly
(lines 337-339). Hallucinated ids are filtered by `resolvePickedImages` (lines
473-496).

**`concept_syntheses` unique on `(repository_id, lower(canonical_name))`.**
`backend/db/migrations/0028_concept_syntheses.up.sql:44-45`:

```sql
CREATE UNIQUE INDEX IF NOT EXISTS uq_concept_syntheses_repo_name
    ON okt_repository.concept_syntheses (repository_id, lower(canonical_name));
```

So there is exactly ONE upsertable synthesis row per canonical-name group per
repo. The worker upserts via `UpsertSynthesis`
(`synthesize_concept.go:418-426`) with `CoveredSummaryIds`, `CoveredConceptIds`,
`EmbeddedImageIds`, and `Model`.

**Synthesis prompt — attribution-grounded + parallel-scenario framing.**
`builtinSynthesisSystemPrompt` in `builtin_prompts.go:304-505` mandates
attribution-grounded tone (Principle 1, lines 312-321), radical source
neutrality (Principle 2, lines 323-330), and a MANDATORY parallel-scenario
structure for contested claims (lines 383-404): build BOTH Scenario A (the
claim is genuine) and Scenario B (the claim is artifact) at full strength as
parallel analyses, and "Only collapse if conclusive" (line 401). Anti-asymmetry
rules at lines 405-421 explicitly forbid one-directional falsifiability and
confident-register asymmetry.

---

## 6. Emergent relations matview

The `concept_relations` materialized view is defined in
`backend/db/migrations/0030_drop_concept_slugs.up.sql:64-77`:

```sql
CREATE MATERIALIZED VIEW okt_repository.concept_relations AS
SELECT
    c1.repository_id,
    lower(c1.canonical_name) AS name_a,
    lower(c2.canonical_name) AS name_b,
    COUNT(DISTINCT fc1.fact_id) AS shared_fact_count
FROM okt_repository.fact_concepts fc1
JOIN okt_repository.concepts       c1 ON c1.id = fc1.concept_id
JOIN okt_repository.fact_concepts  fc2 ON fc2.fact_id = fc1.fact_id
JOIN okt_repository.concepts       c2 ON c2.id = fc2.concept_id
WHERE c1.repository_id = c2.repository_id
  AND lower(c1.canonical_name) < lower(c2.canonical_name)
GROUP BY c1.repository_id, lower(c1.canonical_name), lower(c2.canonical_name)
WITH DATA;
```

**`shared_fact_count`.** The only column beyond the identity triple is
`COUNT(DISTINCT fc1.fact_id) AS shared_fact_count` — the number of facts that
link to both concepts. There are **no typed predicates**: the matview is a pure
co-occurrence count. The relations-list read endpoint ranks by this count.

**Ordered pairs + self-pairs excluded.** The `WHERE lower(c1.canonical_name) <
lower(c2.canonical_name)` clause (line 75) stores pairs as ordered
(lower(name_a) < lower(name_b)) so each unordered pair appears once per repo,
and self-pairs (the same concept joined to itself via two of its
fact_concepts rows) are excluded by the strict `<`.

**Unique index for `REFRESH CONCURRENTLY`.** Lines 79-80:

```sql
CREATE UNIQUE INDEX uq_concept_relations_repo_pair
    ON okt_repository.concept_relations (repository_id, name_a, name_b);
```

`REFRESH MATERIALIZED VIEW CONCURRENTLY` requires a unique index; this one
satisfies it.

**Refresh 10m + `REFRESH CONCURRENTLY`.** The periodic cadence is
`task.refresh_concept_relations_interval: 10m`
(`config.default.yaml:135-142`). The worker
`RefreshConceptRelationsWorker.Work` in
`backend/internal/taskmanager/tasks/refresh_concept_relations.go:126-174`
runs (line 162-164):

```go
pool.Pool.Exec(refreshCtx,
    `REFRESH MATERIALIZED VIEW CONCURRENTLY okt_repository.concept_relations`)
```

`CONCURRENTLY` lets reads proceed while the refresh rebuilds the view in the
background (comment lines 109-114). The job is deduped per-database via River
unique-by-args on `DatabaseName` (lines 51-86) — bursts of `extract_concepts`
batches across repos in the same database coalesce into a single refresh. The
unique `ByState` set EXCLUDES `completed` and `discarded` (lines 77-83) so a
finished refresh frees the slot immediately. `extract_concepts` also enqueues
a refresh at the end of every batch
(`extract_concepts.go:708-738, 543`), best-effort; the periodic tick covers
repos with no recent extraction.

Two supporting indexes (`0030_drop_concept_slugs.up.sql:82-85`) —
`idx_concept_relations_repo_a_count` and `idx_concept_relations_repo_b_count`
— back the per-concept ranked neighbor queries.

---

## 7. Image processing

**`source_images` (inline + page with CHECK constraints).** The
`source_images` table is referenced by `ListSourceImages`
(`source_decomposition.go:536`). Each row carries a `kind` (inline image URL
vs PDF page render), a nullable `url`, a nullable `storage_key`, a nullable
`bytes` length, a nullable `page_number`, and a nullable `alt_text`. The
worker distinguishes the two kinds: inline images come from the
`ImageFetcher`, page renders come from storage (comment lines 586-619).
`imageURLFor` (`source_decomposition.go:747-759`) returns the inline `url`
unchanged, or synthesizes a service-routable
`/api/v1/repositories/{slug}/sources/{sourceID}/images/{imageID}` URL for
page renders (line 757).

**`gemma4:31b-cloud` vision.** Config is `providers.decomposition.image_extraction`
in `config.default.yaml:380-391`:

```yaml
image_extraction:
  enabled: true
  provider: "ollama_cloud"
  model: "gemma4:31b-cloud"
  max_image_bytes: 5242880    # 5 MB
  max_images_per_source: 20
  concurrency: 4
```

So the multimodal model is `gemma4:31b-cloud` via `ollama_cloud`, with a 5 MB
per-image cap and a 20-images-per-source cap. The worker enforces the cap at
`source_decomposition.go:542-546` and logs when it caps.

**`builtinImageFactExtractionPrompt`.** The image prompt is in
`builtin_prompts.go:171-220`. It is formatted with `Source URL`, `Source
title`, and `Image alt text` (lines 173-176), instructs the model to extract
"ONLY atomic, self-contained facts that the image conveys AND that are
relevant to the source topic" (line 180), with a consolidation rule (lines
191-193) and a self-containedness section (lines 195-204). Response shape:
`["fact one", "fact two"]` (line 220).

**`fact_kind='image'`.** The worker persists image facts with
`FactKind: "image"` and `ImageUrl: imgURLPtr` at `source_decomposition.go:682-688`.
The `fact_kind` column was added by
`backend/db/migrations/0016_facts_image.up.sql:19-22`:

```sql
ALTER TABLE okt_repository.facts
    ADD COLUMN IF NOT EXISTS image_url TEXT,
    ADD COLUMN IF NOT EXISTS fact_kind TEXT NOT NULL DEFAULT 'text'
        CHECK (fact_kind IN ('text', 'image'));
```

**`chunk_index=-1`.** Image facts use `chunk_index = -1` on the `fact_sources`
junction (`source_decomposition.go:696-700`) so they sort after text facts in
`ListFactsBySource` (which orders by `chunk_index` then `first_seen_at`) without
a junction schema change (comment lines 692-695).

**Same pipeline as text.** Image facts flow through the same
`embed_facts → deduplicate_facts → extract_concepts → ...` chain because they
are facts (the chain fires on `totalFacts > 0`, line 426, and image facts
mutate `*totalFacts` at line 704). The text and image extraction loops share
the same two-phase pattern: phase 1 fans out with bounded concurrency
(`imageCfg.ConcurrencyOr(4)`, line 570), phase 2 persists serially (lines
642-707).

**Synthesis image picker.** `synthesize_concept.go` loads the group's image
facts via `ListGroupImageFacts` (lines 323-327), converts via
`imageRowsToInputs` (lines 453-467), and when candidates exceed `max_images`
calls `synthesizer.PickImages` (lines 341-356) with `MaxImages: maxImages`.
The picked images are filtered by `resolvePickedImages` (lines 473-496) against
the candidate set to defend against hallucinated ids.

**`embeddedImageIDRe`.** After the synthesis LLM call, the worker extracts
embedded image fact_ids from the markdown via the regex at
`synthesize_concept.go:528`:

```go
var embeddedImageIDRe = regexp.MustCompile(`!\[[^\]]*\]\(<(?:fact:)?([0-9a-fA-F-]{36})>\)`)
```

It matches `![alt](<fact:uuid>)` markdown image citations (and tolerates the
legacy bare-`<uuid>` form). `extractEmbeddedImageIDs` (lines 535-556) returns
the deduplicated list, which the worker stores on
`concept_syntheses.embedded_image_ids` (line 424) so the GET `/definition`
endpoint can eager-load those facts' `image_url` without parsing markdown on
every read (comment lines 398-406).

---

## 8. Auto-annotation

`AnnotateReportWorker` in
`backend/internal/taskmanager/tasks/annotate_report.go` (`Work` at line 123).
Config is `providers.reports` in `config.default.yaml:467-518`:

```yaml
reports:
  enabled: true
  similarity_threshold: 0.7
  lexical_similarity_floor: 0.6
  max_facts_per_sentence: 5
  min_sentence_runes: 40
  posture_classifier:
    enabled: true
    provider: "openrouter"
    model: "google/gemma-4-31b-it"
    batch_size: 8
    max_concurrent: 4
    max_tokens: 800
```

**`decomposition.SegmentSentences` (same chunker).** The worker chunks the
report body into sentences via `decomposition.SegmentSentences(report.BodyMd)`
at `annotate_report.go:236` — the same deterministic chunker the source
pipeline uses, so `sentence_index` keys are stable across re-runs (comment
lines 233-235). A `min_sentence_runes: 40` filter (line 222, applied at lines
239-242) drops short sentences before embedding.

**Threshold `0.7` config / `0.84` code fallback.** The worker resolves the
per-repo override at lines 163-191. The config default is `0.7` (config line
479). The code fallback when no per-repo setting exists is
`w.reportsCfg.SimilarityThresholdOr(0.84)` at line 159. The per-repo row, when
present and `*setting.SimilarityThreshold > 0`, overrides the global (lines
164-166). So: configured default `0.7`, code fallback `0.84`.

**Hybrid lexical fallback.** For each candidate sentence that has at least one
numeric token, the worker runs a lexical (tsvector) search over the repo's
facts and unions hits with the Qdrant hits (lines 312-429). The numeric token
pattern is `numericTokenPattern` (lines 768-770):

```go
var numericTokenPattern = regexp.MustCompile(
    `(?:\d+(?:[.,]\d+)?%?)` +
        `(?:\s*(?:kg|kcal|g|mg|ml|l|mmol|µg|ug|ng|lb|in|cm|mm|m|km|h|min|s|ms|kpa|pa)?)`)
```

**`unitAliases`.** Lines 784-806 map short unit tokens to their long-form
equivalents (e.g. `"kg": {"kilogram", "kilograms"}`) so the lexical fallback
bridges unit spelling variants the english tsvector config stems differently
(comment lines 772-783).

**`extractNumericTsquery`.** Lines 835-878 build a Postgres tsquery string with
` & ` (AND) semantics, quoting numeric tokens so the `.` doesn't break
tsquery parsing, and OR'ing unit aliases together: e.g. "0.9 kg" →
`'"0.9" & ( "kg" | "kilogram" | "kilograms" )'` (example at lines 828-829).

**Apples-to-oranges gate `0.6`.** `lexical_similarity_floor: 0.6` (config line
493) is the semantic-distance gate. Each lexical hit is re-checked against the
sentence embedding via `GetFactVectorsByIDs` (lines 379-390) and
`cosineSimilarity` (lines 903-916); hits below the floor are dropped (lines
409-415) even when the tsvector match was exact, so "0.9 kg weight gain" does
NOT surface "0.9 kg CO2 emissions" purely on the numeric token match (comment
lines 320-335). Setting the floor to 0.0 disables the gate (line 372).

**Posture 4 labels.** When the posture classifier is active
(`postureClassifier.Configured()`, line 437), each (sentence, candidate fact)
pair is sent to the LLM, which labels it one of four postures
(`builtinPostureSystemPrompt`, `builtin_prompts.go:578-597`):
**supports, contradicts, related, irrelevant** (lines 583-586). Irrelevant
pairs are dropped before persistence (the classifier drops them inside
`parseClassifications`; the worker only persists pairs in the classifier
output, lines 529-566).

**Batch 8, concurrent 4.** The classifier batches by fact count
(`appendBatches`, lines 726-741) with `batch_size: 8` (config line 516,
`BatchSizeOr(8)` at line 481). Batches run with bounded concurrency
(`max_concurrent: 4`, config line 517, `MaxConcurrentOr(4)` at line 508) via a
semaphore channel of size `maxConc` (lines 660-700).

**`report_annotations` table.** `backend/db/migrations/0031_reports.up.sql`
defines `reports` (lines 25-40, status CHECK at line 31:
`('pending','processing','annotated','failed')`) and `report_annotations`
(lines 55-63) with `PRIMARY KEY (report_id, sentence_index, fact_id)`, a
`score` column (cosine similarity Qdrant returned), a nullable `posture`
column (added later — the worker writes it at lines 549-556 and 584-591), and
CASCADE on both sides. The worker inserts via `AddReportAnnotation`.

**Per-batch keep-all fallback.** A batch failure logs and falls back to
keep-all for that batch — the affected sentences' Qdrant hits are persisted
with `posture = NULL` (the legacy behavior, lines 571-602). The whole-report
fallback when the classifier can't run at all is the same path (comment lines
431-442). So a flaky LLM call doesn't fail the whole report.

---

## 9. 2-phase workflow + compounding

The ingestion pipeline is **7 stages deep** (documented in
`.opencode/agent/okt.md:144-146` and `.opencode/agent/research.md:49-52`):

```
retrieve_source → source_decomposition → embed_facts → deduplicate_facts
  → extract_concepts → {embed_concepts, summarize_concepts → synthesize_concept}
```

plus `refresh_concept_relations` fanning out from `extract_concepts`
(`extract_concepts.go:543, 708-738`) and `cleanup_facts` chaining after
`embed_concepts` (`embed_concepts.go:221-239`).

**`fetchAndProcessSource` re-enters ingestion.** The MCP tool
`fetchAndProcessSource` is implemented by `handleFetchAndProcessSource` in
`backend/internal/api/handler/mcp.go:1718-1816`. It classifies the URL/DOI
(`fetch.ClassifyURL`, line 1777), enqueues a `retrieve_source` job via
`m.taskEnqueuer.EnqueueRetrieveSourceFromHTTP` (line 1783), and — critically —
sets `Process: true` (line 1796). The comment at lines 1787-1795 explains: this
chains the `retrieve_source` job into `source_decomposition` once the fetch
lands with parseable text, so a single MCP call runs the full retrieve →
decompose → embed pipeline. The tool is named `fetchAndProcessSource` (line
406) and its description promises "extracts facts, and links them"; without
`Process: true` the worker would only fetch and the agent would have to call a
separate decompose step that does not exist as an MCP tool.

**Idempotency.** The whole chain is idempotent at multiple layers:
- `AddFactSource` has an `ON CONFLICT` clause so re-processing a source is a
  no-op for existing fact-source links (`source_decomposition.go:350-357`).
- `CreateFact` is idempotent on content hash; `AddFactSource` is idempotent on
  `(fact_id, source_id)` (see `backend/internal/api/handler/remote_pull.go:81-84`).
- `extract_concepts` takes no advisory lock; the `fact_concepts` unique index
  on `(fact_id, concept_id)` and the `NOT EXISTS (fact_concepts)` filter in
  `ListStableFactsForConceptExtraction` make duplicate work across concurrent
  enqueues a no-op (`extract_concepts.go:174-184`).
- `concept_syntheses` is unique on `(repository_id, lower(canonical_name))`
  (migration 0028, lines 44-45) so `UpsertSynthesis` is idempotent.

**Drain protocol.** The orchestrator and research agents MUST verify the
pipeline has drained before synthesizing. The protocol (`.opencode/agent/okt.md:159-166`,
`.opencode/agent/research.md:96-118`) is:
1. Poll `getSourceTasks(repository, investigationId, verbose=false, limit=200)`.
2. Page through EVERY `next_cursor`, accumulating `pending_count` across pages.
3. Only declare drained when the FINAL page has `next_cursor` empty AND
   `pending_count=0` (the summary is PER-PAGE, not global —
   `research.md:88-94`).
4. Verify the terminal stage ran: filter by `kind=synthesize_concept` and
   confirm those jobs are completed.
5. Sleep between polls scaled to source count: 1 source ≈ 5 min, 10 ≈ 10-20
   min, 50 ≈ 30-60 min, 100 ≈ 1-2 h (`okt.md:217-224`). NEVER sleep < 5 minutes.

**Compounding.** A single source fans out into hundreds of jobs (one embed +
dedup + extract per fact, one summarize + synthesize per concept), so 100
sources can produce tens of thousands of jobs and take an HOUR or more to
drain (`okt.md:147-156`). The pipeline is a directed acyclic graph where each
stage chains to the next via `river.ClientFromContext[pgx.Tx](ctx).Insert`
with a fresh background context (see e.g. `source_decomposition.go:426-445`,
`deduplicate_facts.go:380-394`, `extract_concepts.go:500-514`).

---

## 10. Multi-concept fact querying

The `searchFacts` MCP tool accepts a `concepts` parameter — a list of 2-20
concept names. The handler `handleSearchFacts` in
`backend/internal/api/handler/mcp.go:724-876` validates the bounds (lines
785-807):

```go
if len(concepts) < 2 {
    return mcp.NewToolResultError("`concepts` requires at least 2 entries"), nil
}
if len(concepts) > 20 {
    return mcp.NewToolResultError("`concepts` accepts at most 20 entries"), nil
}
```

After dedup by lowercased name (lines 791-804), the handler resolves each
concept to its group's concept_ids via `m.resolveConcept` (line 816) and
builds two parallel arrays: `allIDs []pgtype.UUID` (the flattened concept_ids)
and `allGroups []int32` (the group index 0..n-1 each concept_id belongs to,
line 822). Then it calls `ListSharedFactsByConceptGroups` (lines 835-844) with
`Column4: allIDs, Column5: allGroups`.

**`ListSharedFactsByConceptGroups` SQL.** `backend/db/queries/facts.sql:420-468`:

```sql
SELECT f.id, f.text, f.status, f.embedded_at, f.embedded_model, f.created_at,
       f.fact_kind, f.image_url,
       COALESCE(fs_count.source_count, 0) AS source_count,
       MIN(fs.source_id::text)::uuid AS source_id
FROM okt_repository.facts f
JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
JOIN okt_repository.sources s ON fs.source_id = s.id
LEFT JOIN (
    SELECT fact_id, COUNT(*) AS source_count
    FROM okt_repository.fact_sources
    GROUP BY fact_id
) fs_count ON fs_count.fact_id = f.id
JOIN (
    SELECT fc.fact_id, COUNT(DISTINCT g2.gv) AS covered_groups
    FROM okt_repository.fact_concepts fc,
         LATERAL unnest($4::uuid[]) WITH ORDINALITY AS c1(cid, c1_idx),
         LATERAL unnest($5::int4[]) WITH ORDINALITY AS g2(gv, g2_idx)
    WHERE c1.c1_idx = g2.g2_idx
      AND c1.cid = fc.concept_id
    GROUP BY fc.fact_id
) cov ON cov.fact_id = f.id
WHERE s.repository_id = $1
  AND ($2::text = '' OR f.status = $2)
  AND ($3::text = '' OR f.search_tsv @@ websearch_to_tsquery('english', $3))
  AND cov.covered_groups = (SELECT COUNT(DISTINCT g) FROM unnest($5::int4[]) AS t(g))
GROUP BY f.id, fs_count.source_count
ORDER BY
    CASE WHEN $6 = 'source_count' THEN COALESCE(fs_count.source_count, 0) END DESC NULLS LAST,
    f.created_at DESC
LIMIT $7 OFFSET $8;
```

**Parallel arrays + `unnest WITH ORDINALITY`.** The two input arrays
(`$4::uuid[]` concept_ids, `$5::int4[]` group indices) are unnested in
parallel via `LATERAL unnest(...) WITH ORDINALITY AS c1(cid, c1_idx)` and
`LATERAL unnest(...) WITH ORDINALITY AS g2(gv, g2_idx)`, joined positionally by
`c1.c1_idx = g2.g2_idx` (line 456). This pairs each concept_id with its group
index. The `WHERE c1.cid = fc.concept_id` (line 457) restricts to
fact_concepts rows whose concept_id is in the input set.

**`cov.covered_groups = COUNT(DISTINCT)`.** The coverage subquery groups by
`fc.fact_id` and computes `COUNT(DISTINCT g2.gv) AS covered_groups` (line 452)
— the distinct set of input group indices the fact is linked to. The outer
query's filter `cov.covered_groups = (SELECT COUNT(DISTINCT g) FROM
unnest($5::int4[]) AS t(g))` (line 463) keeps only facts whose covered group
count equals the total distinct group count in the input — i.e. the fact is
linked to at least one concept_id from EVERY group. This is the N-ary
intersection (comment lines 420-433). `COUNT(DISTINCT i.group_idx)` dedupes a
fact linked to multiple contexts of the same group (comment line 428).

A companion `CountSharedFactsByConceptGroups` (lines 470-490) uses the same
intersection filter minus the ORDER BY / LIMIT / OFFSET for the total count.

---

## 11. Agent layer

**`opencode.jsonc`.** `/home/charlie/Documents/workspace/love/open-knowledge-tree-go/opencode.jsonc`:

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "default_agent": "okt",
  "mcp": {
    "open-knowledge-tree": {
      "type": "remote",
      "url": "http://localhost:8080/api/v1/mcp",
      "enabled": true,
      "timeout": 15000,
      "oauth": { "scope": "mcp", "callbackPort": 19876 }
    }
  }
}
```

The `okt` agent is the default. The MCP server at
`http://localhost:8080/api/v1/mcp` is wired as a remote MCP with OAuth (scope
`mcp`, callback port 19876).

**`.opencode/agent/*.md` — orchestrator + 4 subagents.** The agent directory
contains five markdown files:

| File | `mode` | Role |
|------|--------|------|
| `okt.md` | `primary` | The orchestrator (`model: ollama/glm-5.2:cloud`, `okt.md:4`). |
| `research.md` | `subagent` | Plans + gathers evidence (`research.md:3`). |
| `investigation.md` | `subagent` | Gathers sources on a topic. |
| `synthesizer.md` | `subagent` | Produces a standalone synthesis document (`synthesizer.md:3`). |
| `super-synthesizer.md` | `subagent` | Combines multiple sub-syntheses into a meta-synthesis. |

The orchestrator's decision table (`okt.md:92-98`) lists the four subagents:
`research`, `investigation`, `synthesizer`, `super-synthesizer`. The
orchestrator dispatches via the Task tool with `subagent_type`
(`okt.md:88-106`).

**Information-level tiering.** The orchestrator classifies user intent into
GATHER / SYNTHESIZE / META-SYNTHESIZE / LOOKUP / MIXED (`okt.md:196-233`) and
routes to the right workflow pattern:
- **Pattern A** — focused single-scope synthesis (`okt.md:259-273`).
- **Pattern A+** — planned multi-scope synthesis: `research` subagent
  partitions, then one `synthesizer` per scope in parallel, optional
  `super-synthesizer` (`okt.md:275-313`). Preferred for non-trivial topics.
- **Pattern B** — ad-hoc multi-scope meta-synthesis (`okt.md:315-335`).
- **Pattern C** — add a single source (`okt.md:337-345`).
- **Pattern D** — quick concept/fact lookup (`okt.md:347-350`).

The `research` subagent does Phase 1 (Plan, ~20% budget) + Phase 2 (Gather,
~55%) + Phase 3 (Validate, Repair, Report, ~25%) (`research.md:225-306`). The
`synthesizer` subagent does Phase 1 (Broad Discovery, ~20%) + Phase 2
(Structural Mapping, ~50%) + Phase 3 (Deep Evidence Gathering, ~25%) + Phase 4
(Verify and Write, ~5%) (`synthesizer.md:96-174`).

**Drain protocol.** Both the orchestrator and the research subagent enforce the
MANDATORY drain protocol (see topic 9) before declaring evidence ready
(`okt.md:159-166`, `research.md:96-118`). The orchestrator must run it before
dispatching synthesizers; the research subagent must run it per scope before
reporting.

**File-based handoff.** For super-synthesis flows, the orchestrator MUST use a
file-based handoff protocol (`okt.md:365-397`):
1. Create a temp directory `/tmp/opencode/synthesis-<topic-slug>/`.
2. In EVERY synthesizer task prompt, instruct it to write its complete document
   to a markdown file at `<path>` using the Write tool (`okt.md:378-383`).
3. In the super-synthesizer task prompt, instruct it to read each file
   completely using the Read tool (`okt.md:384-391`).
4. Do NOT pass the sub-synthesis text inline — pass file paths
   (`okt.md:392-394`). Inline produces truncated/summarized inputs and poor
   integration.
5. If a sub-synthesis is truncated in the response, read the file — the file
   has the complete text (`okt.md:395-397`).

The synthesizer's Phase 4 step 21 makes writing to the file MANDATORY
(`synthesizer.md:169-174`).

---

## 12. Existing benchmark scaffolding

**`scripts/experiments/` contents.** The directory holds three Python
experiment scripts plus supporting data and reports:

| File | Purpose |
|------|---------|
| `disambiguation_benchmark.py` (1238 lines) | Tests whether a reduced context list preserves the disambiguation power of the original 789-label DBpedia L3 list. Crafts ambiguous facts (same surface concept, different meaning), sends each to `google/gemma-4-31b-it` via OpenRouter twice (original 789-label prompt vs candidate list), compares the (concept, context) pairs, scores whether each list assigns the right disambiguating context. Outputs `disambiguation_benchmark.json` + stdout summary. Uses `OPENROUTER_URL = "https://openrouter.ai/api/v1/chat/completions"` and `CHAT_MODEL = "google/gemma-4-31b-it"` (lines 45-46). `DEFAULT_ORIGINAL = .../backend/internal/providers/ontology/dbpedia_l3.json`, `DEFAULT_CANDIDATE = .../manual_select.json` (lines 38-42). |
| `merge_labels.py` (323 lines) | Embeds all 789 DBpedia L3 labels with `google/gemini-embedding-2` via OpenRouter (falls back to `BAAI/bge-large-en-v1.5` locally), agglomeratively clusters by cosine distance, and merges until target cluster counts `[789, 400, 200, 150, 100, 50]` (line 55). Outputs `label_merging_result.json`, `merge_dendrogram.png`, `label_projection.png` (2D cluster scatter at target 150, line 56). |
| `dedup-threshold-sweep/dedup_threshold_sweep.py` + `README.md` | Standalone experiment that connects directly to local Qdrant (6333 REST) and Postgres (5432) to replay the `deduplicate_facts` worker's nearest-neighbor search for every embedded fact of a single investigation, then sweeps candidate thresholds to estimate how many facts would be marked `to_delete` at each level. Read-only. Defaults: `--thresholds 0.80,0.82,...,0.98`, `--top-k 20`, `--concurrency 8`, `--examples-per-band 6`. Outputs `<out>.html` + `<out>.json`. Report shows two views: any-neighbor (upper bound) and cross-source-only (matches the current worker). Existing report: `reports/dedup_sweep_shadow_fleet.html` (139 sources, 4,427 embedded text-stable facts, `gemini-embedding-2`, 12.5s sweep). |
| `dbpedia_l3.json` (14022 bytes) | The DBpedia L3 ontology class list — a JSON list of **789** labels. The source of truth for the concept-extraction context vocabulary. |
| `manual_select.json` | The curated reduced list — 88 categories (the `categories` array). Used as `DEFAULT_CANDIDATE` by `disambiguation_benchmark.py`. |
| `disambiguation_benchmark.json` (135633 bytes) | Full results from a disambiguation benchmark run. |
| `label_merging_result.json` (334878 bytes) | Full merge map at every target count from a `merge_labels.py` run. |
| `label_projection.png`, `merge_dendrogram.png` | Visualization outputs from `merge_labels.py`. |

**`backend/e2e/` — 53 test files.** The e2e suite at
`/home/charlie/Documents/workspace/love/open-knowledge-tree-go/backend/e2e/`
contains **53** `*_test.go` files (plus a `testutil/` directory). The full list
(matching topics in this report):

`admin_tasks_test.go`, `admin_test.go`, `ai_embedding_test.go`, `ai_test.go`,
`ai_usage_test.go`, `auth_test.go`, `bootstrap_test.go`, `client_test.go`,
`concept_relations_test.go`, `concepts_test.go`, `content_parsing_test.go`,
`content_types_test.go`, `context_mapping_test.go`, `contributor_test.go`,
`decomposition_providers_test.go`, `dedicated_task_db_test.go`,
`embed_dedup_test.go`, `fact_references_test.go`, `fact_sources_test.go`,
`groups_test.go`, `heavy_tiers_test.go`, `helpers_test.go`,
`image_extraction_test.go`, `investigations_test.go`, `mcp_test.go`,
`multi_db_test.go`, `oauth_test.go`, `openalex_test.go`,
`per_repo_routing_test.go`, `promptsets_test.go`, `providers_test.go`,
`refine_test.go`, `registry_search_test.go`, `remote_test.go`,
`report_hybrid_numeric_test.go`, `report_posture_test.go`, `reports_test.go`,
`repositories_test.go`, `repository_settings_test.go`, `search_test.go`,
`serper_test.go`, `shared_facts_test.go`, `source_boilerplate_test.go`,
`source_parsed_test.go`, `sources_test.go`, `storage_test.go`,
`summaries_test.go`, `syntheses_test.go`, `taskmanager_test.go`,
`tasks_test.go`, `unpaywall_test.go`, `upload_test.go`, `users_test.go`.

Notable files mapped to this report's topics: `embed_dedup_test.go` (topics 1,
4), `fact_references_test.go` (topic 1), `fact_sources_test.go` (topic 1),
`concepts_test.go` (topic 2), `refine_test.go` (topic 3), `concept_relations_test.go`
(topics 2, 6), `summaries_test.go` (topic 5), `syntheses_test.go` (topics 5, 7),
`image_extraction_test.go` (topic 7), `report_hybrid_numeric_test.go` + `report_posture_test.go`
(topic 8), `shared_facts_test.go` (topic 10), `mcp_test.go` (topics 9, 10, 11),
`sources_test.go` (topic 9). Per the AGENTS.md testing policy, every new feature
or behavior change MUST update the matching e2e file; a change is not complete
until `just test-e2e` is green.

---

## Quick-reference: topic → file → key value

| # | Topic | Primary file:line(s) | Key value |
|---|-------|----------------------|-----------|
| 1 | Fact decomposition | `backend/configs/config.default.yaml:357-358, 360-361`; `backend/internal/taskmanager/tasks/source_decomposition.go:296-311, 336-376, 782-794`; `backend/internal/promptset/builtin_prompts.go:22-169`; `backend/db/migrations/0013_facts.up.sql:40-62` | chunk_size 2000, overlap 200, model `google/gemma-4-31b-it`, 9 fact types, `[Sn]` prefixes, `promptset_hash`, `fact_references` sentence provenance, statuses new/stable/to_delete, no `source_id` column (N:M via `fact_sources`) |
| 2 | Concept extraction + contexts + aliases | `backend/internal/taskmanager/tasks/extract_concepts.go:197-285`; `backend/internal/concepts/resolve.go:62-94, 101-205, 213-248`; `backend/db/migrations/0023_concepts.up.sql:24-58`; `scripts/experiments/dbpedia_l3.json` (789 labels); `scripts/experiments/manual_select.json` (88 categories); `backend/configs/config.default.yaml:837-873` (scientific ~30) | unique on `(repository_id, lower(canonical_name), lower(context))`; per-repo `repository_contexts`; DBpedia L3 789 / 88 general / ~30 scientific; `concept_aliases`; per-fact `ResolveAliasMatchForFact` with cosine tie-break; embed-in-place from `canonical_name + " " + context` |
| 3 | Alias merging / pruning | `backend/internal/taskmanager/tasks/refine_concepts.go:136-530, 537-604, 629-643`; `backend/internal/promptset/builtin_prompts.go:274-302`; `backend/configs/config.default.yaml:633-649` | `mergeCandidateIntoConcept`, `applyPruning`, `aliases_to_add`/`aliases_to_prune`, `prune_threshold: 5`, three-stage routing pre-LLM → LLM → post-LLM → create, `max_concurrency: 5` |
| 4 | Fact deduplication | `backend/internal/taskmanager/tasks/deduplicate_facts.go:118-401, 418-451`; `backend/configs/config.default.yaml:434-437, 459-466` | `google/gemini-embedding-2` 3072-dim, threshold `0.94` cosine, stable-wins, new-vs-new order-dependent tie-break, `mergeSources` (fact_sources idempotent, fact_references non-overlapping preserved, fact_concepts relinked), `pg_advisory_xact_lock(hashtext(repository_id))`, `fact_catchup` 168h |
| 5 | Hierarchical summarization + synthesis | `backend/internal/taskmanager/tasks/summarize_concepts.go:124-218, 280-303, 354-456, 513-557`; `backend/internal/taskmanager/tasks/synthesize_concept.go:100-447, 584-670`; `backend/db/migrations/0026_concept_summaries.up.sql:46-48`; `backend/db/migrations/0028_concept_syntheses.up.sql:44-45`; `backend/internal/promptset/builtin_prompts.go:304-505`; `backend/configs/config.default.yaml:595-620, 650-691` | summarization `batch_size: 20` batch-only mode + open accumulator + `is_complete` + unique partial index one open slice per concept; synthesis group key, no-delta skip (`coversAll`), related top 10 + top 3 syntheses verbatim, image picker max 10; `concept_syntheses` unique on `(repository_id, lower(canonical_name))`; prompt attribution-grounded + parallel-scenario framing |
| 6 | Emergent relations matview | `backend/db/migrations/0030_drop_concept_slugs.up.sql:64-85`; `backend/internal/taskmanager/tasks/refresh_concept_relations.go:126-174`; `backend/configs/config.default.yaml:135-142` | `concept_relations` SQL with `shared_fact_count`, no typed predicates, refresh 10m, `REFRESH CONCURRENTLY`, ordered pairs (`lower(a) < lower(b)`), self-pairs excluded, unique index `uq_concept_relations_repo_pair` |
| 7 | Image processing | `backend/internal/taskmanager/tasks/source_decomposition.go:522-711, 747-769`; `backend/internal/taskmanager/tasks/synthesize_concept.go:321-365, 453-496, 528-556`; `backend/internal/promptset/builtin_prompts.go:171-220`; `backend/db/migrations/0016_facts_image.up.sql:19-22`; `backend/configs/config.default.yaml:380-391, 681-682` | `source_images` (inline + page with CHECK constraints), `gemma4:31b-cloud` vision (max 5MB, max 20/source), `builtinImageFactExtractionPrompt`, `fact_kind='image'`, `chunk_index=-1`, same pipeline as text, synthesis image picker, `embeddedImageIDRe` |
| 8 | Auto-annotation | `backend/internal/taskmanager/tasks/annotate_report.go:123-631, 768-878, 903-916`; `backend/internal/promptset/builtin_prompts.go:578-597`; `backend/db/migrations/0031_reports.up.sql:25-66`; `backend/configs/config.default.yaml:467-518` | `decomposition.SegmentSentences` (same chunker), threshold `0.7` config / `0.84` code fallback, hybrid lexical fallback (numeric tokens, `unitAliases`, `extractNumericTsquery`), apples-to-oranges gate `0.6`, posture 4 labels (batch 8, concurrent 4), `report_annotations` table, per-batch keep-all fallback |
| 9 | 2-phase workflow + compounding | `backend/internal/api/handler/mcp.go:1718-1816`; `.opencode/agent/okt.md:144-166, 217-224`; `.opencode/agent/research.md:49-118`; `backend/internal/taskmanager/tasks/source_decomposition.go:426-445`; `backend/internal/taskmanager/tasks/deduplicate_facts.go:380-394`; `backend/internal/taskmanager/tasks/extract_concepts.go:500-514`; `backend/internal/taskmanager/tasks/embed_concepts.go:221-239` | 7-stage pipeline (retrieve_source → source_decomposition → embed_facts → deduplicate_facts → extract_concepts → {embed_concepts, summarize_concepts → synthesize_concept} + refresh_concept_relations + cleanup_facts), `fetchAndProcessSource` re-enters ingestion with `Process: true`, idempotency (AddFactSource ON CONFLICT, CreateFact content hash, fact_concepts unique index, concept_syntheses unique), drain protocol (page through next_cursor, final page pending_count=0, verify synthesize_concept completed, sleep scaled to source count) |
| 10 | Multi-concept fact querying | `backend/internal/api/handler/mcp.go:724-876`; `backend/db/queries/facts.sql:420-490` | `searchFacts` `concepts` parameter 2-20, `ListSharedFactsByConceptGroups` SQL with parallel arrays (`$4::uuid[]`, `$5::int4[]`) + `unnest WITH ORDINALITY` positional join + `cov.covered_groups = COUNT(DISTINCT g) FROM unnest($5)` |
| 11 | Agent layer | `opencode.jsonc`; `.opencode/agent/okt.md` (primary, `model: ollama/glm-5.2:cloud`); `.opencode/agent/research.md`; `.opencode/agent/investigation.md`; `.opencode/agent/synthesizer.md`; `.opencode/agent/super-synthesizer.md` | `opencode.jsonc` (default_agent `okt`, remote MCP at `localhost:8080/api/v1/mcp` with OAuth scope `mcp` / callback 19876); orchestrator + 4 subagents (research, investigation, synthesizer, super-synthesizer); information-level tiering (GATHER/SYNTHESIZE/META-SYNTHESIZE/LOOKUP/MIXED) + Patterns A/A+/B/C/D; drain protocol; file-based handoff (`/tmp/opencode/synthesis-<slug>/`, Write to file, Read by super-synthesizer) |
| 12 | Existing benchmark scaffolding | `scripts/experiments/disambiguation_benchmark.py`; `scripts/experiments/merge_labels.py`; `scripts/experiments/dedup-threshold-sweep/{dedup_threshold_sweep.py,README.md,reports/}`; `scripts/experiments/{dbpedia_l3.json, manual_select.json, disambiguation_benchmark.json, label_merging_result.json, label_projection.png, merge_dendrogram.png}`; `backend/e2e/` (53 `*_test.go` files) | `disambiguation_benchmark.py` (789-label vs candidate list, `google/gemma-4-31b-it`); `merge_labels.py` (789→[400,200,150,100,50] via `google/gemini-embedding-2` + scipy clustering); `dedup_threshold_sweep` (Qdrant+Postgres read-only, thresholds 0.80-0.98, any-neighbor vs cross-source-only views); `dbpedia_l3.json` 789 labels; `manual_select.json` 88 categories; 53 e2e test files covering every topic |
