# Scope A — OKT System Architecture (Code-Grounded)

This scope describes *what OKT is* as a running system — the architectural
commitments, the ingestion pipeline, the emergent knowledge graph, the agent
layer, and the auditability surface — and where each mechanism introduces
fragility. Every claim is cited to a code path (file.go:line, migration:line,
or config:line) so the reader can verify it. Exact thresholds, prompts, and SQL
live in the companion Scope E report
(`.opencode/okt-paper-scopes/scopeE-implementation.md`); this scope
synthesizes the architecture around them rather than restating them.

---

## 1. Architectural commitments

OKT is a Go 1.22+ service (`AGENTS.md`) whose source of truth is **PostgreSQL
16**, with **Qdrant** confined to the role of a dumb vector store. The
`facts` and `concepts` tables, the `fact_sources` / `fact_references` /
`fact_concepts` junctions, the `concept_summaries` / `concept_syntheses`
rows, and the `concept_relations` materialized view all live in the
`okt_repository` schema
(`backend/db/migrations/0013_facts.up.sql`, `0023_concepts.up.sql`,
`0026_concept_summaries.up.sql`, `0028_concept_syntheses.up.sql`,
`0030_drop_concept_slugs.up.sql`). Qdrant holds only embedding vectors with
a payload of `{repository_id, status}` — no text, no concept metadata, no
graph structure (`backend/configs/config.default.yaml:449-458`;
`UpdateFactStatusPayload` at `deduplicate_facts.go:361-365`). The Qdrant
collection is fully reconstructable from Postgres: given a fact or concept
row, its vector can be re-embedded and re-upserted; given a vector, the
payload only routes searches by repo and filters by status.

The HTTP layer is **Chi** over a layered `internal/api/` tree
(`AGENTS.md` "Folder Structure"). SQL is **sqlc**-generated: hand-written
queries in `backend/db/queries/*.sql` become typesafe Go in
`backend/internal/store`, eliminating hand-rolled store boilerplate. RBAC
is **Casbin** with a custom pgx adapter
(`backend/internal/rbac/adapter.go`), policies stored as
`casbin_rule` rows. Background work is **River**, the job queue whose
seven-stage ingestion pipeline (Section 11) is the spine of the system.
The frontend is **SolidJS** with Vite/Tailwind; it speaks to the same API
the agent layer uses. The "Page Size Policy" in `AGENTS.md` enforces
frontend modularization, mirroring the backend's separation of transport,
domain, and store layers.

Two consequences follow from this stack. First, the **system is
transactional**: every fact write, dedup merge, and synthesis upsert happens
inside a Postgres transaction (e.g. the per-repo advisory lock in
`deduplicate_facts.go:149-164`). Qdrant is never a transaction participant
— its updates are best-effort post-commit, and a Qdrant outage degrades
search but not storage. Second, **the schema is the spec**: migrations
encode the invariants (the `facts` table has no `source_id` column per
`0013_facts.up.sql:7-12`; `concept_summaries` has a partial unique index
guaranteeing one open slice per concept per
`0026_concept_summaries.up.sql:46-48`; `concept_syntheses` is unique on
`(repository_id, lower(canonical_name))` per `0028:44-45`). The code reads
those invariants as axioms.

A multi-database layout (`AGENTS.md` "Multi-database layout") lets repos
share a tier-1 Postgres or sit in isolated tier-2/3 databases; the same
migration set runs against every declared database, and a per-connection
`search_path` of `okt_system, okt_repository, public` is set by the dbpool
registry's `AfterConnect` hook. This is the substrate on which the rest of
the architecture stands.

## 2. Fact decomposition instead of chunking

The decisive departure from RAG is at the first ingestion stage.
`SourceDecompositionWorker.Work`
(`backend/internal/taskmanager/tasks/source_decomposition.go:122`) does
**not** chunk a source into passages to be retrieved verbatim. It runs a
two-phase extract-then-persist loop (lines 286-382) that asks the LLM to
emit **atomic, self-contained facts** from each chunk.

**Nine fact types.** The `builtinFactExtractionPrompt`
(`backend/internal/promptset/builtin_prompts.go:22-169`) enumerates nine
fact types — `claim, account, measurement, formula, quote, procedure,
reference, code, perspective` (lines 27-44) — each with a description,
length hint, and worked example. The prompt also encodes
self-containedness rules (lines 65-97), a SKIP list (lines 83-96), and the
response shape `[{"text":"...","sentences":[0,1]}]` (lines 167-169).

**Sentence-level provenance via `[Sn]` prefixes.** When the retrieve
worker has persisted a sentence array for the source, each chunk is
rewritten by `buildLabeledChunkText` (`source_decomposition.go:782-794`)
to emit only the global sentences overlapping the chunk's rune range,
each prefixed with its global `[Sn]` index. The model echoes those
indices; the worker writes them directly to `fact_references` with no
chunk-relative translation (lines 362-376). Hallucinated out-of-range
indices are silently dropped — the fact survives, the bad reference does
not (lines 362-365).

**The `facts` table has no `source_id` column.** Migration
`0013_facts.up.sql:7-12` is explicit: "There is no 'origin' concept and no
`facts.source_id` column — all source links live in the junction." The
N:M link is `fact_sources` (lines 56-62) with `PRIMARY KEY (fact_id,
source_id)` and a per-extraction `chunk_index`. `AddFactSource`'s
`ON CONFLICT` clause (`source_decomposition.go:350-357`) makes the link
idempotent on re-process or dedup merge. This is what allows a fact to be
supported by *many* sources and *many* sentences across them, without a
denormalized "primary source" column.

**Status lifecycle.** The `status` column is constrained to `('new',
'stable', 'to_delete')` (`0013_facts.up.sql:43-44`). `new` = freshly
extracted; `stable` = survived dedup; `to_delete` = flagged as duplicate
and reaped later by the 168h `fact_catchup` periodic job
(`config.default.yaml:466`, `deduplicate_facts.go:339`).

**`promptset_hash` as philosophy tag.** The worker resolves the repo's
effective promptset once at `Work()` start
(`source_decomposition.go:154-163`) and stamps that hash into every fact
row (line 336-341) and every `fact_references` row (line 366-372). Two
different extraction philosophies (e.g. one that extracts claims only, one
that also extracts procedures) therefore produce facts that never mix in
the same provenance pool — a `promptset_hash` mismatch is a hard barrier
to cross-philosophy dedup, by design.

**Why facts not chunks.** Four properties chunks lack: (1) *deduplicatable*
— atomic facts at cosine ≥ 0.94 collapse onto one survivor (Section 5),
whereas overlapping chunks resist exact collapse; (2) *concept-linkable*
— a fact maps to one or more `(canonical_name, context)` concepts, a
chunk does not; (3) *citable* — a synthesis sentence can cite a fact by
UUID and the frontend resolves it to its supporting sources, a chunk
citation would have to resolve to a substring; (4) *queryable* — the
`searchFacts` `concepts` intersection (Section 10) finds facts at the
*intersection* of concept groups, a chunk-level search cannot express
that structurally. The cost is dependence on extraction LLM quality
(Section 14).

## 3. Concepts, contexts, and per-fact disambiguation

A concept is a `(repository_id, canonical_name, context)` triple. The
`concepts` table carries a case-insensitive composite uniqueness index on
exactly those three (`0023_concepts.up.sql:42-43`), so the identity of a
concept is `(repository_id, lower(canonical_name), lower(context))`. The
same name in different contexts is a *different* concept: `N/Nitrogen`
and `N/Neutron` are two rows, not one row with a polymorphic type.

**Contexts are a curated DBpedia-L3 vocabulary.** The allowed context set
is per-repo, stored in `repository_contexts` and loaded at
`extract_concepts.go:275-285`. An empty list hard-fails (lines 279-281):
the admin must configure contexts via the repository-settings UI. The
builtin concept-extraction prompt (`builtinConceptExtractionPrompt`,
`builtin_prompts.go:222-272`) is formatted with the L3 ontology class
list (lines 254-256) and instructs: "The context MUST be one of the labels
in the list above, verbatim" (line 261). Vocabulary sizes per Scope E:
**789** raw DBpedia L3 labels (`scripts/experiments/dbpedia_l3.json`),
**88** curated "general" categories (`scripts/experiments/manual_select.json`,
resolved to the full ~88 at creation per `config.default.yaml:820-825`),
and ~30 "scientific" contexts per the scientific repository preset
(`config.default.yaml:837-873`). The backfill migration seeds every
legacy repo with the full vocabulary.

**Aliases.** `concept_aliases` (`0023_concepts.up.sql:45-50`) carries
alternate surface forms, unique case-insensitively per concept (lines
57-58), with a `lower(alias_text)` lookup index (lines 51-52) backing the
text-search match path. Seed aliases are emitted by the extraction model
and merged for free via `AddConceptAlias`'s `ON CONFLICT DO NOTHING`
(`extract_concepts.go:828-839, 946-960`).

**The decision is per-fact: "to which concept does THIS fact belong?"**
When an alias is shared by multiple concepts (the canonical "N" example),
`ResolveAliasMatchForFact` (`backend/internal/concepts/resolve.go:62-94`)
picks, for *one specific fact*, the concept whose Qdrant vector is
cosine-closest to that fact's vector. Strategy (lines 41-61):
`FindConceptsByAlias` → 0 matches is a miss; 1 match returns it; >1
matches invokes `disambiguateByEmbedding` (lines 101-205), which fetches
the fact's vector and the candidates' vectors, embeds-in-place any
candidate missing a vector (lines 152-176), and cosine-compares. The
in-place embedding input is `c.CanonicalName + " " + c.Context`
(`embedConceptInPlace`, `resolve.go:224`). Disambiguation is therefore a
*per-fact* decision grounded in the actual semantic content of that
fact, not a per-alias vote. This is the structural feature that makes an
ambiguous surface form resolvable in a shared fact pool — and it is also
where the embedding model's representational bounds become the
disambiguation ceiling (Section 14).

## 4. Alias merging and pruning

`RefineConceptsWorker` (`backend/internal/taskmanager/tasks/refine_concepts.go:136`)
resolves unresolved `concept_candidates` through a three-stage routing
loop documented in the `Work` doc comment (lines 118-131) and implemented
in `refineOneCandidate` (lines 282-530):

1. **Pre-LLM (all DB, no LLM)** — canonical match (step 1, line 307),
   single-target alias merge (step 2, lines 319-333), per-fact
   `tryRouteAliasAmbiguous` fork when >1 alias match, seed-alias match
   (step 3, lines 336-362).
2. **LLM call** — only when all pre-LLM routes miss: one
   `refiner.Refine` call with a 120s context (lines 365-384), gated by a
   per-canonical advisory lock `TryAdvisoryLockForSynthesis` (lines
   393-404) so two goroutines don't race on the same AI canonical.
3. **Post-LLM (all DB, reuses LLM output)** — AI canonical match (step
   4, line 414), AI alias match (step 5, lines 424-456).
4. **Create** — if all miss, `CreateConcept` (line 459) with the AI
   canonical + AI aliases + seed aliases, then reassign facts and
   resolve the candidate.

The LLM proposes `aliases_to_add` and `aliases_to_prune` in the JSON shape
`{"canonical_name":"...","aliases_to_add":[...],"aliases_to_prune":[...]}`
(`builtinRefinementPrompt`, `builtin_prompts.go:274-302`, response shape
at 300-301). `mergeCandidateIntoConcept` (lines 537-604) moves the
candidate's facts onto the target, copies seed and AI aliases, adds the
candidate's `concept_text` itself as an alias, resets the target's
embedding, and resolves the candidate in a resolved-candidate cache.
`applyPruning` (lines 629-643) deletes each alias in `pruneList` via
`DeleteConceptAliasByText`, counting `AliasesPruned` atomically. The
re-prune gate is `prune_threshold: 5` (`config.default.yaml:638`): an
established concept's aliases are re-pruned only when ≥ 5 new aliases
have accumulated since the last refinement (config comment 631-632).
Concurrency is capped at `max_concurrency: 5` via an errgroup `SetLimit`
(line 239, `NewRefineConceptsWorker` line 105).

The fragility here is concentrated in step 2: the LLM canonicalization
is gated by a per-canonical advisory lock, but the *quality* of the
canonical name is the LLM's judgment call. A bad canonical name pollutes
the canonical-name group forever after (the `concept_syntheses` row is
unique on `lower(canonical_name)`, so renaming a canonical after the
fact is a schema-level disruption). The refinement worker is also
**off by default** (`config.default.yaml:633`: `enabled: false`), so
candidate accumulation until an operator enables it is a real failure
mode.

## 5. Fact deduplication via embedding distance

`DeduplicateFactsWorker` (`deduplicate_facts.go:118`) runs once the
embedding pass completes. The embedding model is
`google/gemini-embedding-2` at 3072 dimensions
(`config.default.yaml:434-437`); vectors live in the shared `okt_facts`
Qdrant collection with a `{repository_id, status}` payload. The dedup
threshold is `0.94` cosine (`config.default.yaml:459-466`), read at
`deduplicate_facts.go:255` and passed to `qdrant.SearchSimilarByID` with
`limit=1`.

**Stable-wins.** The loop sorts stable-first, then new,
UUID-ascending within each group (lines 189-200). For each `new` fact,
it searches Qdrant for the nearest neighbor excluding self, score ≥
threshold, limit 1. The `switch hitStatus` (lines 272-333):
- `stable` → mark the new fact `to_delete`, merge its sources onto the
  stable survivor (lines 273-289). **Stable always wins over new.**
- `new` → the current fact wins; the hit is marked `to_delete`
  immediately and `statusByID[hit.ID]` is mutated so the loop skips it
  when reached later (lines 290-329). This is the order-dependent
  new-vs-new tie-break (doc comment lines 108-117) — the previous
  "lex-larger UUID loses" rule was deterministic but symmetric and
  missed same-batch twins; the new rule catches them at the cost of
  order dependence on the UUID sort.
- `to_delete` → skip (lines 330-332).

**`mergeSources` preserves all references.** The merge routine
(lines 418-451) is the dedup contract: `AddFactSource` per loser source,
idempotent via the junction's `ON CONFLICT` clause (lines 423-431, the
loser's `chunk_index` is preserved);
`DeleteDuplicateFactReferences` drops the loser's citations that would
collide with the winner's existing `(source_id, sentence_index)` rows
(lines 432-436); `RelinkFactReferences` moves the remaining rows onto
the winner (lines 437-443) — **non-overlapping citations from both
facts are preserved** (doc lines 412-417); `RelinkFactConcepts` relinks
the loser's `fact_concepts` rows (lines 444-449). The guarantee: after a
merge, the surviving fact carries the union of both facts' sources,
the union of both facts' non-colliding sentence references, and the
union of both facts' concept links.

**Per-repo advisory lock.** The whole pass runs in one transaction
(lines 149-153) holding `pg_advisory_xact_lock(hashtext(repository_id))`
(line 162). The lock is transaction-scoped, so two enqueues for the same
repo always serialize on their `hashtext(repository_id)`; two repos in
different databases dedup in parallel. After dedup, surviving `new`
facts promote to `stable` via `MarkFactsStableByRepo` (line 339) and
their Qdrant payloads flip via `UpdateFactStatusPayload` (lines
361-365). The worker chains to `extract_concepts` (lines 380-394).

The `0.94` threshold is a **tuning knob**, not a law. Scope E notes the
`dedup-threshold-sweep` experiment
(`scripts/experiments/dedup-threshold-sweep/`) that replays the
worker's nearest-neighbor search across thresholds 0.80–0.98 to
estimate `to_delete` rates. The right value is corpus-dependent; a
single global threshold cannot be optimal across both a dense
scientific corpus (where paraphrases are near-identical) and a
heterogeneous web corpus (where the same fact surfaces in very
different phrasings).

## 6. Hierarchical summarization and final synthesis

Above the fact pool sit two layers of LLM-generated text:
per-concept **summary slices** and per-canonical-name-group
**syntheses**. Together they form a layered hierarchy the agent layer
navigates: facts → concepts → summaries → synthesis.

**Summarization.** `SummarizeConceptsWorker` (`summarize_concepts.go:124`)
runs in **batch-only mode** once a concept has at least one complete
(frozen) summary slice: only emit complete `batch_size: 20` slices,
never an open remainder, and skip the pass when fewer than 20 new facts
arrived (`summarize_concepts.go:280-303`). While no complete slice
exists yet, the worker also emits one open remainder
(`is_complete=FALSE`) as the incremental accumulator (lines 354-374);
once a complete slice is written, the loop breaks so leftover facts
stay uncovered until a full batch gathers. The invariant — *at most one
open slice per concept* — is enforced by a partial unique index
(`0026_concept_summaries.up.sql:46-48`) and the worker relies on it:
`GetOpenSummary` is a scalar lookup. The per-concept lock is
`concepts.summarizing_at` (migration lines 56-57), claimed via
`ClaimConceptForSummary` with a 2h staleness window
(`summarize_concepts.go:236-250`). The first slice absorbs the
per-fact regeneration cost while a concept is small; afterward the
worker waits for a full batch before spending another LLM call.

**Synthesis.** `SynthesizeConceptsWorker` (`synthesize_concept.go:100`)
is **on by default** (`config.default.yaml:650`), model
`deepseek/deepseek-v4-flash:turbo`. The group key is `repoIDStr + ":" +
strings.ToLower(concept.CanonicalName)` (line 214); the worker resolves
the concept_id to its canonical-name group, loads ALL summary slices
across the group (`ListSummariesByCanonicalNameGroup`, lines 238-241)
and the group's concept_ids (`ListGroupConceptIDs`, lines 252-255). A
**no-delta skip** avoids redundant regeneration: the existing synthesis
is loaded via `GetSynthesisByGroup` (lines 264-267) and the worker skips
when `coversAll(existing.CoveredSummaryIds, slices)` (lines 273-276).
The `concept_syntheses` table is unique on `(repository_id,
lower(canonical_name))` (`0028_concept_syntheses.up.sql:44-45`), so
there is exactly one upsertable synthesis row per group per repo, and
`UpsertSynthesis` (lines 418-426) is idempotent.

**Related concepts block.** `loadRelatedConcepts` (lines 584-670)
loads the top `max_related_concepts: 10` related concepts by
`shared_fact_count` from the `concept_relations` matview via
`ListConceptRelationsByConceptName` (lines 599-604). Of those, the top
`max_related_syntheses: 3` by rank also carry their existing synthesis
text verbatim via `GetSynthesisByGroup` (lines 654-665). The synthesis
LLM therefore reasons not only about a concept's own facts but about its
three strongest neighbor syntheses — a structural amplification step
that lets one concept's prior synthesis inform another's.

**Image picker.** Image candidates are loaded via
`ListGroupImageFacts` capped at `max_image_candidates: 50` (lines
323-327); when candidates exceed `max_images: 10`, the picker LLM call
runs (`PickImages`, lines 341-356) with `MaxImages: 10`, otherwise all
candidates pass through (lines 337-339). Hallucinated ids are filtered by
`resolvePickedImages` (lines 473-496). After the synthesis call, the
worker extracts embedded image fact_ids from the markdown via
`embeddedImageIDRe` (`synthesize_concept.go:528`) and stores them on
`concept_syntheses.embedded_image_ids` (line 424) so reads don't have
to parse markdown.

**The synthesis prompt is the heart of the system's epistemic stance.**
`builtinSynthesisSystemPrompt` (`builtin_prompts.go:304-505`) mandates
**attribution-grounded tone** (Principle 1, lines 312-321) — every
assertion connected to who or what supports it; **radical source
neutrality** (Principle 2, lines 323-330) — no source gets to make bare
unattributed claims regardless of institutional prestige; and a
**MANDATORY parallel-scenario structure** for contested claims (lines
383-404): both Scenario A (the claim is genuine) and Scenario B (the
claim is artifact) must be built at full strength as parallel analyses,
and "Only collapse if conclusive" (line 401). Anti-asymmetry rules
(lines 405-421) explicitly forbid one-directional falsifiability and
confident-register asymmetry between mainstream and alternative
readings. This is the only place in the stack where OKT's
epistemic commitments are *prompt-encoded* rather than
schema-encoded — which makes them the most fragile commitment of all
(Section 14).

## 7. The emergent concept graph

Concepts **emerge from facts**; there is no preexisting ontology. The
extraction worker assigns facts to `(canonical_name, context)` concepts
that it invents on the fly within the constraint of the context
vocabulary; the refinement worker later collapses duplicates. A repo
with no facts about `X` has no `X` concept — `searchConcepts(query="X")`
returns nothing, `getConcept` errors, and the `concept_relations`
matview has no row involving `X`. Adding the first fact that the
extraction model assigns to `X` is what creates the concept.

**Contexts are the only top-down element.** The vocabulary of allowed
contexts is curated (789 / 88 / ~30, Section 3), but within a context,
the canonical names are entirely emergent. This is a deliberate hybrid:
a closed context vocabulary gives the system a stable disambiguation
axis (the per-fact cosine tie-break would be useless if "Nitrogen" and
"Neutron" shared a context), while an open canonical-name space lets
the ontology grow with the corpus.

The graph is therefore a **projection of the fact pool**. If a fact is
deleted (`to_delete` → reaped), its `fact_concepts` rows go with it, and
on the next matview refresh the concept's `shared_fact_count` edges
update. The graph cannot drift from the facts — it is computed from
them. The cost is that the graph also cannot represent *anything the
extraction model missed*: a fact that the model failed to extract is
invisible to every downstream stage, and a concept the model failed to
name is invisible to every downstream navigation.

## 8. The `concept_relations` materialized view

The relations layer is a Postgres materialized view
(`0030_drop_concept_slugs.up.sql:64-77`):

```sql
CREATE MATERIALIZED VIEW okt_repository.concept_relations AS
SELECT c1.repository_id,
       lower(c1.canonical_name) AS name_a,
       lower(c2.canonical_name) AS name_b,
       COUNT(DISTINCT fc1.fact_id) AS shared_fact_count
FROM okt_repository.fact_concepts fc1
JOIN okt_repository.concepts      c1 ON c1.id = fc1.concept_id
JOIN okt_repository.fact_concepts fc2 ON fc2.fact_id = fc1.fact_id
JOIN okt_repository.concepts      c2 ON c2.id = fc2.concept_id
WHERE c1.repository_id = c2.repository_id
  AND lower(c1.canonical_name) < lower(c2.canonical_name)
GROUP BY c1.repository_id, lower(c1.canonical_name), lower(c2.canonical_name)
WITH DATA;
```

**`shared_fact_count` is the only column beyond the identity triple** —
the number of facts that link to both concepts. There are **no typed
predicates**: the matview is a pure co-occurrence count. The
relations-list read endpoint ranks by this count. The `WHERE lower(c1)
< lower(c2)` clause stores pairs as ordered (lower(name_a) <
lower(name_b)) so each unordered pair appears once per repo and
self-pairs (the same concept joined to itself via two of its
`fact_concepts` rows) are excluded by the strict `<`.

A unique index `uq_concept_relations_repo_pair` on
`(repository_id, name_a, name_b)` (lines 79-80) is required for
`REFRESH MATERIALIZED VIEW CONCURRENTLY`, which the
`RefreshConceptRelationsWorker` runs every 10m
(`task.refresh_concept_relations_interval: 10m`,
`config.default.yaml:135-142`; `refresh_concept_relations.go:126-174`,
the `REFRESH` at line 162-164). `CONCURRENTLY` lets reads proceed while
the refresh rebuilds in the background (comment lines 109-114). The job
is deduped per-database via River unique-by-args on `DatabaseName`
(lines 51-86) so bursts of `extract_concepts` batches across repos in
the same database coalesce into a single refresh. `extract_concepts`
also enqueues a refresh at the end of every batch
(`extract_concepts.go:543, 708-738`), best-effort.

**The structural departure from triplet KGs.** A triplet KG
(`subject, predicate, object`) encodes a typed relation as a first-class
edge. OKT encodes no predicates at all: the edge is "concepts that share
facts" weighted by how many. This trades expressive power for
verifiability — every `shared_fact_count` is reproducible by
`SELECT count(DISTINCT fact_id) FROM fact_concepts WHERE concept_id IN
(...)`. There is no LLM-generated `predicate` label whose correctness
must be argued; there is no ontology maintenance burden. The cost is
that semantic relations ("X causes Y", "X contradicts Y") are not
expressible at the graph level. They surface instead at the synthesis
level, as prose, where the posture classifier (Section 13) can label
them per (sentence, fact) pair — but they are not *queryable* as
graph edges.

## 9. A novel KG structure (not triplets)

Taken together, Sections 2–8 describe a knowledge graph that is not a
triplet store:

- **Facts are first-class atoms.** A fact is a row with a UUID, text,
  an embedding, a status, a `promptset_hash`, an optional `image_url`
  and `fact_kind`. It is not a literal in a triple; it is the unit of
  epistemic weight.
- **Concepts are many-per-name.** The same canonical name in different
  contexts is a different concept row. `(N, Nitrogen)` and `(N,
  Neutron)` coexist without a polymorphic type or a disambiguation
  prefix.
- **Relations are derived, not asserted.** `concept_relations` is a
  matview over `fact_concepts`; it has no independence from the fact
  pool. Adding a fact updates the graph on the next refresh; deleting
  a fact retracts the edge. The graph is **always consistent with the
  fact pool** by construction.

The graph is therefore a *projection of the fact pool* rather than a
separately authored structure. This is the architectural feature that
underwrites OKT's claim to auditability: any graph-level assertion can
be reduced in constant hops to the facts that support it and to the
sources and sentences that support those facts. There is no separate
"graph author" whose judgment must be trusted.

## 10. Agent navigation at multiple information levels

The agent layer is an opencode deployment with five markdown agents
(`.opencode/agent/*.md`) wired to a remote MCP server at
`http://localhost:8080/api/v1/mcp` with OAuth scope `mcp`
(`opencode.jsonc`). The **orchestrator** (`okt.md`, `mode: primary`,
`model: ollama/glm-5.2:cloud`) dispatches four **subagents**:
`research` (plans + gathers), `investigation` (gathers sources),
`synthesizer` (produces a standalone synthesis document), and
`super-synthesizer` (combines multiple sub-syntheses)
(`okt.md:92-98`). The orchestrator classifies user intent into
GATHER / SYNTHESIZE / META-SYNTHESIZE / LOOKUP / MIXED (`okt.md:196-233`)
and routes to one of five workflow patterns (A/A+/B/C/D,
`okt.md:259-350`).

**The 16 MCP tools** (`backend/internal/api/handler/mcp.go`,
registered at lines enumerated in `AGENTS.md`) are the only surface the
agents have to the knowledge graph: `getRepositories`,
`searchFacts`, `getFact`, `searchConcepts`, `getConcept`,
`getConceptSummaries`, `getRelatedConcepts`, `getInvestigation`,
`createInvestigation`, `searchSources`, `listSearchProviders`,
`fetchAndProcessSource`, `getSourceTasks`, `createReport`, `getReport`,
`listReports`, `getReportTasks`. They expose the layered hierarchy
*directly*: facts → concepts → summaries → synthesis. An agent navigates
*down* (`getConcept` → `getRelatedConcepts` → `getConcept` on a
neighbor → ...) and *up* (`searchFacts` → `getFact` → its linked
concepts → ...). Information-level tiering is enforced per agent:
the research subagent does Phase 1 Plan (~20% budget) + Phase 2 Gather
(~55%) + Phase 3 Validate/Repair/Report (~25%) (`research.md:225-306`);
the synthesizer does Phase 1 Broad Discovery (~20%) + Phase 2
Structural Mapping (~50%) + Phase 3 Deep Evidence Gathering (~25%) +
Phase 4 Verify and Write (~5%) (`synthesizer.md:96-174`).

**`searchFacts` `concepts` intersection is the verification primitive.**
The handler `handleSearchFacts` (`mcp.go:724-876`) accepts a `concepts`
parameter — a list of 2-20 concept names (bounds enforced at lines
785-807). After dedup by lowercased name (lines 791-804), it resolves
each concept to its group's concept_ids via `m.resolveConcept` (line
816) and builds two parallel arrays: `allIDs []pgtype.UUID` (flattened
concept_ids) and `allGroups []int32` (the group index 0..n-1 each
concept_id belongs to, line 822). The SQL
`ListSharedFactsByConceptGroups` (`backend/db/queries/facts.sql:420-468`)
unnests both arrays in parallel with `WITH ORDINALITY`, joins them
positionally by `c1.c1_idx = g2.g2_idx`, restricts to `fact_concepts`
rows whose concept_id is in the input set, groups by `fact_id` and
computes `COUNT(DISTINCT g2.gv) AS covered_groups`, then filters the
outer query to facts whose `covered_groups` equals the total distinct
group count — i.e. the fact is linked to at least one concept_id from
*every* group. This is the N-ary intersection: "find facts at the
intersection of these N concept groups." A single SQL query expresses a
multi-concept verification that would require multiple joins or a
client-side loop in a triplet KG.

**Mandatory drain protocol.** Both the orchestrator and the research
subagent MUST verify the pipeline has drained before synthesizing
(`okt.md:159-166`, `research.md:96-118`):
1. Poll `getSourceTasks(repository, investigationId, verbose=false,
   limit=200)`.
2. Page through EVERY `next_cursor`, accumulating `pending_count`
   across pages — the summary is **per-page, not global**
   (`research.md:88-94`).
3. Only declare drained when the FINAL page has `next_cursor` empty AND
   `pending_count=0`.
4. Verify the terminal stage ran: filter by `kind=synthesize_concept`
   and confirm those jobs are completed.
5. Sleep between polls scaled to source count: 1 source ≈ 5 min, 10 ≈
   10-20 min, 50 ≈ 30-60 min, 100 ≈ 1-2h (`okt.md:217-224`). NEVER sleep
   < 5 minutes.

Skipping this protocol means synthesizing against partial facts
(still `new`, not yet deduped) and missing concepts — the resulting
synthesis will be subtly wrong and the agent will not know why.

## 11. The 2-phase workflow and compounding

OKT has two phases. **Phase 1** is the seven-stage ingestion pipeline
(`.opencode/agent/okt.md:144-146`, `.opencode/agent/research.md:49-52`):

```
retrieve_source → source_decomposition → embed_facts → deduplicate_facts
  → extract_concepts → {embed_concepts, summarize_concepts → synthesize_concept}
```

plus `refresh_concept_relations` fanning out from `extract_concepts`
(`extract_concepts.go:543, 708-738`) and `cleanup_facts` chaining after
`embed_concepts` (`embed_concepts.go:221-239`). Each stage chains to the
next via `river.ClientFromContext[pgx.Tx](ctx).Insert` with a fresh
background context (e.g. `source_decomposition.go:426-445`,
`deduplicate_facts.go:380-394`, `extract_concepts.go:500-514`). The
pipeline is a directed acyclic graph; a single source fans out into
hundreds of jobs (one embed + dedup + extract per fact, one summarize +
synthesize per concept), so 100 sources can produce tens of thousands
of jobs and take an hour or more to drain (`okt.md:147-156`).

**Phase 2** is research via the MCP tools and reports: an agent
navigates the graph (`searchFacts`, `getConcept`,
`getRelatedConcepts`), adds new sources (`fetchAndProcessSource`), and
writes reports (`createReport`).

**The compounding loop.** `fetchAndProcessSource`
(`mcp.go:1718-1816`) is the bridge between Phase 2 and Phase 1. It
classifies the URL/DOI (`fetch.ClassifyURL`, line 1777), enqueues a
`retrieve_source` job via `EnqueueRetrieveSourceFromHTTP` (line 1783),
and — critically — sets `Process: true` (line 1796). The comment at
lines 1787-1795 explains: this chains `retrieve_source` into
`source_decomposition` once the fetch lands, so a single MCP call runs
the full retrieve → decompose → embed pipeline. *A research agent in
Phase 2 can re-enter Phase 1 at any time*, and the new facts compound
into the existing pool. Next time the same agent (or another) searches,
the new facts are there, deduped against the old, linked to concepts,
summarized, synthesized.

**Idempotency is what makes compounding safe.** The whole chain is
idempotent at multiple layers: `AddFactSource`'s `ON CONFLICT` clause
(`source_decomposition.go:350-357`) makes re-processing a source a
no-op for existing fact-source links; `CreateFact` is idempotent on
content hash; the `fact_concepts` unique index on `(fact_id,
concept_id)` plus a `NOT EXISTS (fact_concepts)` filter in
`ListStableFactsForConceptExtraction` make duplicate `extract_concepts`
work a no-op (`extract_concepts.go:174-184`); `concept_syntheses` is
unique on `(repository_id, lower(canonical_name))` so `UpsertSynthesis`
is idempotent; `AddReportAnnotation` is `PRIMARY KEY (report_id,
sentence_index, fact_id)` so re-annotation overwrites in place.

**Contrast with one-shot RAG and end-to-end agents.** A one-shot RAG
system retrieves chunks at query time and discards them after
generation; the next query re-retrieves from the same corpus, with no
memory of the last. An end-to-end agent (browse → reason → answer)
similarly discards its intermediate work. OKT *keeps* every fact,
every concept link, every synthesis. The cost is the ingestion
pipeline; the benefit is that the second query against the same corpus
is answered against a richer, deduplicated, concept-linked pool than
the first, and the hundredth against a pool that has been compounded
by every prior research session.

## 12. Image processing → fact pool

Image facts are first-class facts, not a separate stream. The
`source_images` table (referenced by `ListSourceImages` at
`source_decomposition.go:536`) carries a `kind` (inline image URL vs
PDF page render), a nullable `url`, `storage_key`, `bytes`,
`page_number`, `alt_text`. `imageURLFor` (`source_decomposition.go:747-759`)
returns the inline `url` unchanged, or synthesizes a service-routable
`/api/v1/repositories/{slug}/sources/{sourceID}/images/{imageID}` URL
for page renders (line 757). The vision model is `gemma4:31b-cloud`
via `ollama_cloud`, with a 5 MB per-image cap and a 20-images-per-source
cap (`config.default.yaml:380-391`), enforced at
`source_decomposition.go:542-546`.

The image prompt `builtinImageFactExtractionPrompt`
(`builtin_prompts.go:171-220`) is formatted with `Source URL`,
`Source title`, and `Image alt text`, instructing the model to extract
"ONLY atomic, self-contained facts that the image conveys AND that are
relevant to the source topic" (line 180), with a consolidation rule
(lines 191-193) and self-containedness section (lines 195-204). Response
shape `["fact one", "fact two"]` (line 220).

Image facts persist with `FactKind: "image"` and `ImageUrl: imgURLPtr`
(`source_decomposition.go:682-688`). The `fact_kind` column was added by
`0016_facts_image.up.sql:19-22` with a CHECK constraint
`fact_kind IN ('text', 'image')`. Image facts use `chunk_index = -1` on
the `fact_sources` junction (`source_decomposition.go:696-700`) so they
sort after text facts in `ListFactsBySource` (which orders by
`chunk_index` then `first_seen_at`) without a junction schema change
(comment lines 692-695). Critically, image facts flow through the *same*
`embed_facts → deduplicate_facts → extract_concepts → ...` chain because
they are facts (the chain fires on `totalFacts > 0`, line 426, and image
facts mutate `*totalFacts` at line 704). A chart and its caption can
therefore dedup against each other, link to the same concepts, and
appear in the same synthesis.

The synthesis image picker (Section 6) loads the group's image facts
via `ListGroupImageFacts` and runs `PickImages` when candidates exceed
10. The synthesis prompt can cite images with
`![alt](<fact:uuid>)` markdown, and the worker extracts those citations
post-hoc via `embeddedImageIDRe` (`synthesize_concept.go:528`) and
stores them on `concept_syntheses.embedded_image_ids` (line 424) so
reads don't have to parse markdown on every request.

## 13. Auto-annotation / auditability

`AnnotateReportWorker` (`annotate_report.go:123`) is the auditability
mechanism. When an agent writes a report via `createReport`, the worker
chunks the report body into sentences via `decomposition.SegmentSentences`
— the *same deterministic chunker the source pipeline uses*
(`annotate_report.go:236`, comment lines 233-235), so `sentence_index`
keys are stable across re-runs. A `min_sentence_runes: 40` filter
(line 222, applied at 239-242) drops short sentences before embedding.

**Embedding match at 0.7.** The configured threshold is `0.7`
(`config.default.yaml:479`); the code fallback when no per-repo setting
exists is `0.84` (`annotate_report.go:159`, `SimilarityThresholdOr`).
The per-repo row, when present and `*setting.SimilarityThreshold > 0`,
overrides the global (lines 164-166). For each sentence, the worker
queries Qdrant for the nearest facts above threshold and persists hits
to `report_annotations` (`backend/db/migrations/0031_reports.up.sql:55-63`,
`PRIMARY KEY (report_id, sentence_index, fact_id)`, a `score` column
holding the cosine Qdrant returned, a nullable `posture` column).

**Hybrid lexical fallback.** For each candidate sentence with at least
one numeric token, the worker runs a lexical (tsvector) search over the
repo's facts and unions hits with the Qdrant hits (lines 312-429). The
numeric token pattern is `numericTokenPattern` (lines 768-770), which
matches numbers with optional units (kg, kcal, g, mg, ml, mmol, µg, ng,
lb, in, cm, mm, m, km, h, min, s, ms, kpa, pa). `unitAliases` (lines
784-806) maps short unit tokens to their long-form equivalents (e.g.
`"kg": {"kilogram", "kilograms"}`) so the lexical fallback bridges
spelling variants the english tsvector config stems differently.
`extractNumericTsquery` (lines 835-878) builds a Postgres tsquery with
` & ` (AND) semantics, quoting numeric tokens so the `.` doesn't break
tsquery parsing, and OR'ing unit aliases: e.g. "0.9 kg" →
`'"0.9" & ( "kg" | "kilogram" | "kilograms" )'`.

**Apples-to-oranges gate.** `lexical_similarity_floor: 0.6`
(`config.default.yaml:493`) is the semantic-distance gate. Each
lexical hit is re-checked against the sentence embedding via
`GetFactVectorsByIDs` (lines 379-390) and `cosineSimilarity` (lines
903-916); hits below the floor are dropped (lines 409-415) even when
the tsvector match was exact, so "0.9 kg weight gain" does NOT surface
"0.9 kg CO2 emissions" purely on the numeric token match (comment
lines 320-335). Setting the floor to 0.0 disables the gate (line 372).

**Posture classifier — 4 labels.** When active
(`postureClassifier.Configured()`, line 437), each (sentence, candidate
fact) pair is sent to the LLM, which labels it one of **supports,
contradicts, related, irrelevant** (`builtinPostureSystemPrompt`,
`builtin_prompts.go:578-597`, lines 583-586). **Irrelevant pairs are
dropped before persistence** (the classifier drops them inside
`parseClassifications`; the worker only persists pairs in the
classifier output, lines 529-566). Batches run with `batch_size: 8`
and `max_concurrent: 4` via a semaphore channel of size `maxConc`
(lines 660-700). A batch failure logs and falls back to keep-all for
that batch — the affected sentences' Qdrant hits are persisted with
`posture = NULL` (the legacy behavior, lines 571-602). A flaky LLM
call doesn't fail the whole report.

**The auditability property.** Every sentence in a report is either
annotated with the fact(s) that support it (with a cosine score and a
posture label) or visibly uncited (no annotation row). Unsupported
prose is surfaced *by absence of annotations*, not by a separate
"verification pass" that could itself be wrong. A reader can scan
`report_annotations` and see exactly which sentences have evidentiary
backing and which do not; an agent can re-run synthesis only on the
uncited sentences. This is the property that underwrites OKT's claim to
address AI hallucination during scientific research: the system does
not *prevent* hallucination, it *surfaces* it, by making the citation
graph a first-class, queryable artifact rather than a post-hoc
explanation.

## 14. Strengths and weaknesses (honest, code-grounded)

**Strengths.**

- **Atomic facts.** Decomposition into 9 typed atomic facts
  (`builtin_prompts.go:22-169`) instead of chunks makes the unit of
  epistemic weight small, deduplicatable, and concept-linkable.
- **Emergent ontology.** Concepts emerge from facts (Section 7); no
  preexisting ontology must be authored or maintained. The context
  vocabulary is the only top-down element, and it is a closed, curated
  list (789 / 88 / ~30).
- **Per-fact disambiguation.** `ResolveAliasMatchForFact`
  (`resolve.go:62-94`) decides concept membership per fact via cosine
  tie-break, not per-alias vote. This is what makes ambiguous surface
  forms resolvable in a shared pool.
- **Sentence-level provenance.** `[Sn]` prefixes
  (`source_decomposition.go:782-794`) + `fact_references` rows give
  every fact a traceable path back to specific sentences in specific
  sources.
- **Dedup-preserves-all-references guarantee.** `mergeSources`
  (`deduplicate_facts.go:418-451`) preserves the union of both facts'
  sources, non-colliding sentence references, and concept links. A
  merge never loses provenance.
- **Auto-annotation with posture.** The 4-label posture classifier
  (`builtin_prompts.go:578-597`) labels each (sentence, fact) pair as
  supports/contradicts/related/irrelevant and drops irrelevant pairs
  before persistence. The auditability property follows by absence.
- **Multimodal facts.** `fact_kind='image'` facts enter the same
  pipeline as text facts (Section 12), so charts and captions dedup
  against each other and share concepts.
- **Idempotent compounding.** `AddFactSource` ON CONFLICT,
  `CreateFact` content hash, `fact_concepts` unique index,
  `concept_syntheses` unique on `(repo, lower(canonical_name))` —
  re-fetching, re-processing, and re-annotating are all no-ops on
  existing data. Compounding is safe.
- **Layered agent navigation.** The 16 MCP tools expose the
  facts → concepts → summaries → synthesis hierarchy directly, and the
  `searchFacts` `concepts` intersection (2-20) is a verification
  primitive no triplet KG offers in one query.

**Weaknesses.**

- **LLM-dependent extraction quality.** The 9-type fact prompt and the
  concept extraction prompt are the only things between a source and
  the fact pool. A fact the extraction model missed is invisible to
  every downstream stage; a fact it extracted badly is a bad atom in
  the pool forever (until reaped by 168h `fact_catchup`). No fallback
  verifies extraction recall.
- **Dedup threshold is a tuning knob.** `0.94` cosine
  (`config.default.yaml:459-466`) is a single global threshold. The
  `dedup-threshold-sweep` experiment
  (`scripts/experiments/dedup-threshold-sweep/`) shows the `to_delete`
  rate varies sharply with threshold; one value cannot be optimal
  across both dense and heterogeneous corpora.
- **Embedding model bounds disambiguation.** The per-fact cosine
  tie-break in `ResolveAliasMatchForFact` is only as good as the
  `gemini-embedding-2` 3072-dim representation. If two senses of a
  concept name embed near each other, disambiguation fails silently.
- **Concept canonicalization depends on the refinement LLM.** The
  refinement worker is **off by default** (`config.default.yaml:633`,
  `enabled: false`) and gated by a per-canonical advisory lock
  (`refine_concepts.go:393-404`). If an operator never enables it,
  `concept_candidates` accumulate unresolved; the canonical-name
  groups that synthesis operates on never get cleaned up. A bad
  canonical name chosen at creation pollutes the
  `(repository_id, lower(canonical_name))` group forever after,
  because `concept_syntheses` is unique on that key.
- **Cost.** One LLM call per chunk-batch (decomposition), per concept
  batch (extraction), per refinement candidate, per summary slice, per
  synthesis, per image picker, per posture batch. A 100-source
  ingestion can produce thousands of LLM calls. The summarization
  batch-only mode (Section 6) and the synthesis no-delta skip
  (`synthesize_concept.go:273-276`) are the only cost caps.
- **No explicit predicate relations.** `concept_relations` is a pure
  co-occurrence count (Section 8). "X causes Y" is not expressible as a
  queryable graph edge; it surfaces only as synthesis prose, where the
  posture classifier can label it per (sentence, fact) pair but cannot
  make it a first-class graph citizen.
- **Emergent ontology can fragment without aggressive pruning.** The
  `prune_threshold: 5` re-prune gate (`config.default.yaml:638`) only
  fires when 5 new aliases accumulate. With refinement off, two
  near-identical concepts can coexist indefinitely, fragmenting
  `shared_fact_count` across both.
- **Qdrant dependency.** Qdrant is not a transaction participant
  (Section 1). A Qdrant outage degrades search but not storage; a
  Postgres-commit-then-Qdrant-failure window leaves vectors stale until
  the next embedding pass. The system is *eventually* consistent
  between Postgres and Qdrant, not atomically.
- **Posture classifier can mislabel.** The 4-label classifier inherits
  the LLM's judgment; the `builtinPostureSystemPrompt`
  (`builtin_prompts.go:578-597`) is the only constraint. AttributionBench
  (per Scope E benchmarking notes) places an ~80% F1 ceiling on this
  kind of posture classification, so ~1 in 5 labels is wrong. A
  mislabel is silent: the annotation row persists with a wrong
  `posture` and no human review is in the loop.

The architecture is therefore not a hallucination *preventer*. It is a
hallucination *surfacers*: a layered system in which every claim can be
traced to a fact, every fact to its sources and sentences, every
concept relation to the shared facts that derive it, and every report
sentence to its supporting annotations or its visible absence of them.
The fragility lives in the LLM calls that gate each layer — extraction,
disambiguation, refinement, summarization, synthesis, posture — and
in the operator-tunable thresholds (`0.94`, `0.7`, `0.6`, `5`, `20`,
`10`) that determine where each layer's decisions land. The code paths
above are the ground truth; the strengths and weaknesses inherit from
which decisions are schema-enforced (the `facts` table has no
`source_id`, the matview has no predicates) and which are prompt- or
threshold-enforced (extraction quality, posture labels, dedup
boundary).