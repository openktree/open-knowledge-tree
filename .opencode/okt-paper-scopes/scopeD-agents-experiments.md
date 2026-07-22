# Scope D — KG+LLM Unification, Autonomous Research Agents, and a Proposed Experimental Programme for OKT

A research document (Scope D of a five-scope paper on the Open Knowledge Tree) situating OKT within the KG+LLM unification and autonomous-research-agent literature, articulating OKT's 2-phase workflow as a paradigm contrasted with one-shot RAG and end-to-end agents, and proposing six locally-runnable experiments with runnable protocols. Companion to Scope E (implementation specifics), which grounds every MCP tool, endpoint, and config value referenced below.

---

## Part 1 — The KG+LLM and Agentic Research Landscape

### 1. Opening

Three architectural responses to the same problem — that LLMs hallucinate and that their reasoning is unauditable — currently dominate the literature: (i) **KG-augmented LLMs**, which ground generation in an external structured knowledge source; (ii) **autonomous end-to-end research agents**, which attempt to produce a complete research artifact in one pass with ephemeral internal state; and (iii) **compounding knowledge substrates**, of which the Open Knowledge Tree (OKT) is an instance, which separate knowledge-building from research and let an agent navigate a persistent, fact-grounded graph at multiple information levels. This document treats OKT and end-to-end agents as **parallel responses** to the hallucination/auditability problem rather than as competitors with one privileged; the question is not "which is right" but "which architectural commitment pays off against which baseline, at what cost, for which question type." To make that question answerable, the document then proposes a concrete, locally-runnable experimental programme of six experiments that pit OKT against named baselines (LlamaIndex chunk retrieval, SelfCheckGPT, FActScore, naive dedup, DBpedia Spotlight, GraphRAG) using existing scaffolding in `scripts/experiments/`.

### 2. Scope & Boundaries

**In scope:** (a) KG+LLM unification literature, anchored on [Unifying Large Language Models and Knowledge Graphs: A Roadmap](<concept:4a085f7e-a557-428b-9d83-718020942504>); (b) autonomous-research-agent literature, anchored on [The AI Scientist](<concept:aa6aa5b5-b69a-42f1-9354-7b37a4f96771>) and [The AI Scientist-v2](<concept:905fd725-6181-457a-a47a-4804f21aff44>); (c) hallucination-detection and citation-evaluation literature ([FActScore](<concept:fe17e23a-4634-4415-9f0b-d0d66d34902c>), [SelfCheckGPT](<concept:2e5b2633-0ee8-4a75-b283-1ecf3520f5e5>), [RAGTruth](<concept:521cf4de-212e-478d-a4f0-633667a2c195>), [Self-RAG](<concept:349d1517-7cc9-442c-be23-59d4283f7f03>), [GraphRAG](<concept:ebfe3a08-c49c-4be8-833a-db67e56f9ab0>), ALCE, RAGAs); (d) OKT's 2-phase workflow as a paradigm contrasted with one-shot RAG and end-to-end agents; (e) six proposed experiments with runnable protocols.

**Out of scope (covered elsewhere):** OKT internals (Scope A — implementation report is Scope E), RAG/hallucination surveys as primary object (Scope B), KG/ontology construction as primary object (Scope C).

**Critical caveat:** No empirical claim is made here about OKT's *current measured* performance against any baseline. The six experiments are **proposed**, not reported; their protocols name actual MCP tools, endpoints, and config values drawn from Scope E, but no results have been run. Honest predictions are stated per experiment and are explicitly labeled as predictions.

### 3. KG-augmented LLMs and LLM-augmented KGs

According to [Pan et al. (2024)](<concept:1142ed6e-591d-435f-aea5-26a3fb4da94a>), the unification of LLMs and KGs decomposes into three frameworks: (1) **KG-enhanced LLMs**, which incorporate KGs during pre-training and inference or to enhance understanding of knowledge learned by LLMs; (2) **LLM-augmented KGs**, which use LLMs for KG tasks including embedding, completion, construction, graph-to-text generation, and question answering; and (3) **Synergized LLMs + KGs**, in which LLMs and KGs work in a mutually beneficial way to provide bidirectional reasoning driven by both data and knowledge ([fact](<fact:3917c17c-6f66-4edf-b736-06fae0bb824f>)). The paper appears in IEEE TKDE, volume 36, issue 7, pages 3580–3599, 2024 ([fact](<fact:f7555e71-38f9-4d78-9766-1bfc680aa08a>)).

Where OKT sits in this taxonomy is the key architectural observation. OKT operates **simultaneously** in frameworks 1 and 2:

- **As framework 2 (LLM-augmented KG):** the ingestion pipeline's `source_decomposition` worker (Scope E §1, model `google/gemma-4-31b-it`, `chunk_size=2000`, `chunk_overlap=200`) uses an LLM to extract atomic facts from source text, and the `extract_concepts` worker (Scope E §2, same model, batch 10, concurrency 4) uses an LLM to assign each fact to a `(canonical_name, context)` concept against a per-repo context vocabulary (DBpedia L3's 789 labels, a curated 88-category list, or a ~30-category scientific preset). The graph is built *by* the LLM, not by a human ontologist.

- **As framework 1 (KG-enhanced LLM):** the agent layer (Scope E §11) navigates the graph via MCP tools — `searchFacts`, `searchConcepts`, `getConcept`, `getRelatedConcepts`, `searchFacts(concepts=[X,Y])` for N-ary concept intersection (Scope E §10). The LLM's research output is grounded in the graph rather than in its parametric memory alone.

The auto-annotation loop (Scope E §8) **closes the cycle**: the LLM builds the graph (framework 2), and the graph then audits the LLM's research output by sentence-embedding a report against the fact pool and persisting matched facts as `report_annotations` with a `posture` (supports/contradicts/related) per (sentence, fact) pair. This is structurally a **synergized** system (framework 3) in Pan et al.'s sense, with the important architectural difference that the synergy is **asynchronous and batched**, not tightly interleaved per token: the graph is built during Phase 1 ingestion, and the LLM is audited against it during Phase 2 research, with the two phases separated by the drain protocol (Scope E §9). This contrasts with systems like [Self-RAG](<concept:349d1517-7cc9-442c-be23-59d4283f7f03>), in which a single language model "adaptively retrieves passages on-demand, and generates and reflects on both the retrieved passages and its own generations using special tokens called reflection tokens" ([fact](<fact:71346ff2-33af-42df-acc4-9587c9a13b52>)) — i.e., tightly interleaved per-token retrieval and critique inside one model. Whether asynchronous-batched synergy or per-token synergy produces better auditability per dollar is an open empirical question (Experiment 2 below addresses one slice).

A second KG+LLM survey in the pool, [LLM-empowered knowledge graph construction: A survey](<concept:d87e0e61-1004-4ba7-b283-8551e202b064>), analyzes how LLMs reshape the classical three-layered pipeline of ontology engineering, knowledge extraction, and knowledge fusion ([fact](<fact:8372702a-e340-4ad5-afcc-d8ad0208a51e>)), and reviews approaches from two perspectives — schema-based (structure, normalization, consistency) and schema-free (flexibility, adaptability, open discovery) ([fact](<fact:0e30ac79-f15c-484e-a5f1-e6ad06626781>)). OKT's concept-extraction sits closer to the schema-based end: the context vocabulary is fixed per-repo (an explicit `repository_contexts` table), the concept identity is `(repository_id, lower(canonical_name), lower(context))`, and the model is instructed "The context MUST be one of the labels in the list above, verbatim" (Scope E §2). But the *canonical names themselves* are emergent — the LLM proposes them from the corpus, not from a top-down entity list — so OKT is a hybrid: top-down *contexts*, bottom-up *entities*. This hybrid is exactly what Experiment 4 (emergent concept quality vs DBpedia Spotlight) is designed to test.

A third related work, "Knowledge Graphs as Context Sources for LLM-Based Explanations of Learning Recommendations" (Abu-Rasheed, Weber, Fathi; IEEE EDUCON 2024) ([fact](<fact:b72f9720-0c01-43e2-a406-969b7d6d5dd8>)), provides a direct precedent for using a KG as a context source for LLM explanation rather than merely as a retrieval index — a use case OKT's `getConcept` synthesis + `getRelatedConcepts` traversal is functionally positioned for.

### 4. Autonomous research agents: Sakana AI Scientist v1 and v2

The AI Scientist v1 ([The AI Scientist](<concept:aa6aa5b5-b69a-42f1-9354-7b37a4f96771>)) was authored by Chris Lu, Cong Lu, Robert Tjarko Lange, Jakob Foerster, Jeff Clune, and David Ha as arXiv:2408.06292, 2024 ([fact](<fact:e62f8221-13f0-46fd-8097-84860c2ade21>)), released by Sakana AI in collaboration with the Foerster Lab at Oxford and Clune/Lu at UBC ([fact](<fact:919875d4-e470-4e64-9659-cde3eab9d0f7>)). According to Sakana AI, the system is designed to be compute-efficient, with each idea implemented and developed into a full paper at a cost of approximately $15 per paper ([fact](<fact:177b7c21-7ec9-402b-a45f-98bf2d68ac53>)), and when combined with the most capable LLMs produces papers that the system's own automated reviewer judges as "Weak Accept" for a top machine learning conference ([fact](<fact:7a7d4d69-a1c6-4596-8c34-4824233c6615>)). The v1 acknowledged limitations are explicitly safety-relevant: it "may incorrectly implement its ideas or create unfair comparisons to baselines, which can lead to misleading results" ([fact](<fact:98b45eaa-54f8-4d87-853a-1b2d476a1aa8>)); it "occasionally makes critical errors when writing and evaluating results, specifically struggling to compare the magnitude of two numbers, a known pathology with Large Language Models" ([fact](<fact:d50ac75c-353e-4cc9-a7cf-725656cec118>)); in one instance "it edited its code to perform a system call to run itself, resulting in the script calling itself endlessly" ([fact](<fact:6c845180-d91c-4a81-80c6-973f4544a312>)); and in another "it attempted to modify its own code to extend the timeout period rather than optimizing the code to run faster" ([fact](<fact:43c15187-f0da-4d5d-b3bb-34c724f22f7c>)).

The AI Scientist v2 ([The AI Scientist-v2](<concept:905fd725-6181-457a-a47a-4804f21aff44>)) was co-led by Yutaro Yamada (shared first author, "coded the core tree-search and template-free version," led the writing) ([fact](<fact:f1ed7a7e-e34b-49c3-b51b-50cc34847c49>)) and introduces a progressive agentic tree search, vision-language model figure refinement, and the elimination of v1's human-authored templates. The v2 evaluation submitted three manuscripts to the ICLR 2025 ICBINB workshop; one achieved an average reviewer score of 6.33 (individual scores 6, 6, 7), exceeding the workshop's acceptance threshold, "marking the first instance of a fully AI-generated paper successfully navigating a peer review" ([fact](<fact:c668b63b-409f-4818-af44-2628a54194a3>)); the team withdrew the accepted paper post-review to avoid setting a precedent ([fact](<fact:3e740e33-77fe-4d67-9a26-66f8c087b47d>)). The v2's own internal inspection found that "the system occasionally introduced inaccuracies in citations, similar to the 'hallucination' issue found in large language models" ([fact](<fact:eecd054d-4d61-4bb0-b25e-d04666c3edaa>)). The v2 concept's structural neighbors (from `getRelatedConcepts`) are dominated by the [Agentic Tree-Search Algorithm](<concept:a2df13fc-f2a9-4dde-8cff-fa1d5f314f74>) (19 shared facts), the [I Can't Believe It's Not Better Workshop](<concept:41ca88c1-7a5f-45bb-816d-77e8b3c13f80>) (15), and the [Vision-Language Model](<concept:f134ae49-6e29-4ddd-a224-7dd2e88ecab0>) (15) — i.e., the v2 concept is structurally tightly coupled to its specific venue and its specific technical components, not to a broad research-literature cluster.

**Paradigmatic difference.** The AI Scientist v1/v2 **is** an agent — it carries ephemeral internal state across idea generation, experimental iteration, write-up, and review, and emits a finished artifact; its intermediate state is not structured as a queryable graph that survives the run. OKT **is not an agent** — it is a knowledge-building pipeline (Phase 1) plus an agent-navigation substrate (Phase 2). The two are not substitutes; an AI Scientist-style agent could in principle be built **on top of** OKT's MCP surface, replacing v1/v2's ephemeral internal scratchpad with the persistent fact/concept graph. This is not a claim any source makes; it is a structural observation about the layering: OKT exposes `fetchAndProcessSource`, `searchFacts`, `getConcept`, `getRelatedConcepts`, `searchFacts(concepts=[...])`, `createReport`, and `getReport` (auto-annotation) as composable MCP tools, where AI Scientist v1 exposes a single end-to-end `generate_paper(starting_code, broad_direction)` procedure.

**Agent hypothesis (not stated by any source).** The v1/v2 reported failure modes — incorrect implementations, unfair baseline comparisons, magnitude-comparison errors, citation hallucinations, code that was written but never executed — are exactly the class of error a verifiable fact-pool substrate is designed to surface: a fact pool makes "this experiment was never run" checkable by the absence of matching atomic facts, and an auto-annotation pass makes "this citation is hallucinated" checkable by the absence of a matching fact in `report_annotations`. The convergence of AI Scientist's self-reported failure modes with OKT's auto-annotation target is a structural rhyme, not a tested result. Confirming it would require building an AI-Scientist-on-OKT variant and running the v2 evaluation protocol — outside the scope of this document but a concrete research direction.

### 5. Hallucination detection and citation evaluation literature

Five lines of work define the evaluation literature OKT's auto-annotation is positioned against:

- **FActScore** ([Min et al. 2023](<concept:52aa3599-f603-4310-a164-08b644058cda>), arXiv:2305.14251, EMNLP 2023 pp. 12076–12100) ([fact](<fact:020f539a-686c-4d04-9f80-c32b8a474ed2>)) ([fact](<fact:d841105a-b89f-4713-8a9d-ef2044941507>)) decomposes a long-form generation into atomic facts and checks each against a knowledge source (Wikipedia). Its authors argue long-form factuality evaluation is non-trivial because "generations often contain a mixture of supported and unsupported information, making binary judgments inadequate, and human evaluation is time-consuming and costly" ([fact](<fact:136446d9-6efb-4a9c-92e1-0dbe199a4bd6>)). OKT's auto-annotation is functionally closest to FActScore — both segment a generation into atomic units and check each against a pool — but the pool in OKT is a **curated internal fact pool built from the same domain** (the repo's `okt_repository.facts` table), not open-web Wikipedia. This is the structural reason to predict OKT wins in-domain and loses out-of-domain (Experiment 2).

- **ALCE** ([Gao et al. 2023](<concept:2efde2c1-e7ec-422f-a0f9-0eed0d0b27cc>), arXiv:2305.14627) ([fact](<fact:6087b0e8-d6ae-4ddb-802f-04789a274b8b>)) is "the first benchmark for Automatic LLMs' Citation Evaluation" ([fact](<fact:1ff1cc64-0249-4b10-9727-d606b638982e>)), with automatic metrics across three dimensions — fluency, correctness, and citation quality — that "correlate strongly with human judgements" ([fact](<fact:f3ef6825-dc0f-4f8c-8e6d-3424d6f38a40>)). The OKT synthesis prompt's mandatory `[description](<fact:factID>)` linking (Scope E §5; synthesis prompt `builtinSynthesisSystemPrompt`, `builtin_prompts.go:304-505`) is a direct instance of the citation-quality dimension ALCE operationalizes.

- **SelfCheckGPT** ([Manakul et al. 2023](<concept:2e5b2633-0ee8-4a75-b283-1ecf3520f5e5>)) is "a black-box zero-resource hallucination detection scheme that operates by comparing multiple sampled responses and measuring consistency" ([fact](<fact:79c2fdf1-e857-4e85-861e-0c5bee058e55>)), premised on the observation that "when a Large Language Model (LLM) has been trained on a given concept, its sampled responses are likely to be similar and contain consistent facts, whereas stochastically sampled responses for hallucinated facts are likely to diverge and may contradict one another" ([fact](<fact:792f2c72-d324-4207-a2c8-91dfb4470903>)). It requires no external knowledge source — it exploits sampling variance. OKT's auto-annotation takes the opposite design choice: it requires an external fact pool but does not depend on sampling variance, so it can detect a hallucination that is *consistently* made (a false claim the model states identically across samples — a class SelfCheckGPT is structurally blind to).

- **RAGTruth** ([Niu et al. 2024](<concept:521cf4de-212e-478d-a4f0-633667a2c195>)) is a "large-scale corpus of naturally generated hallucinations featuring detailed word-level annotations tailored for retrieval-augmented generation (RAG) scenarios" ([fact](<fact:46934bc3-9cff-4e30-b70a-1b670ce6a6cb>)), with 2,965 instances and 17,790 responses across QA, data-to-text, and summarization, and an overall hallucination rate of 43.1% ([fact](<fact:3532f9e5-457e-4c24-87f5-adb4a3a72a04>)). Its authors claim "using a high-quality dataset such as RAGTruth makes it possible to develop specialized hallucination detection models that are more effective than prompt-based methods using general models such as GPT-4" ([fact](<fact:02f2e394-e178-4e94-bfad-3d49bd22f9f3>)) — i.e., a fine-tuned small Llama-2-13B can outperform GPT-4 prompted for the same task. This is a direct challenge to OKT's prompt-based posture classifier (Scope E §8, `google/gemma-4-31b-it`, batch 8, concurrent 4): if a fine-tuned detector beats a prompted GPT-4, it plausibly also beats a prompted gemma-4-31b. RAGTruth is therefore both a baseline dataset and a potential training source for a future OKT posture classifier.

- **GraphRAG** ([Edge et al. 2024](<concept:ebfe3a08-c49c-4be8-833a-db67e56f9ab0>), arXiv:2404.16130 "From Local to Global: A Graph RAG Approach to Query-Focused Summarization") ([fact](<fact:5d1efbd1-2781-44de-abb4-1c32891f9425>)) "uses a large language model (LLM) to build a graph index in two stages: (1) derive an entity knowledge graph from the source documents, and (2) pregenerate community summaries for all groups of closely related entities" ([fact](<fact:5d1efbd1-2781-44de-abb4-1c32891f9425>)). This is the closest structural cousin to OKT in the literature: both build an entity graph from source documents with an LLM and both pre-generate summaries. The differences are (i) GraphRAG's community summaries are cluster-shaped (groups of closely related entities, via community detection) where OKT's syntheses are entity-shaped (one synthesis per canonical-name group, via `concept_syntheses` unique on `(repository_id, lower(canonical_name))`, Scope E §5); (ii) GraphRAG's graph is built for global sensemaking queries, OKT's graph is built for fact-level retrieval plus multi-level navigation. Experiment 6 below tests the two head-to-head on global/theme QA.

- **RAGAs** ([Es et al. 2024](<concept:c12531d7-318e-4905-b1bb-1e6e53ca0671>)) is "a framework for reference-free evaluation of Retrieval Augmented Generation (RAG) pipelines" ([fact](<fact:c1833067-a037-4fa1-ac4b-d9eb11c8f146>)). It is the natural evaluation harness for several of the experiments below (any time OKT and a RAG baseline are compared on a fixed question set, RAGAs metrics apply).

### 6. The 2-phase workflow as a research paradigm

OKT's separation into **Phase 1 (knowledge building)** and **Phase 2 (research)** is a paradigmatic commitment, not an implementation detail.

**Phase 1 — knowledge building.** A 7-stage River pipeline (Scope E §9): `retrieve_source → source_decomposition → embed_facts → deduplicate_facts → extract_concepts → {embed_concepts, summarize_concepts → synthesize_concept}`, plus `refresh_concept_relations` fanning out from `extract_concepts`. It is **compounding**: each new source enriches existing concepts rather than producing a flat index. The marginal LLM cost per new source is **not uniform** — a cache hit on an existing concept (`FindResolvedCandidate` paths, `concept_syntheses` upsert idempotency, no-delta `coversAll` skip in `synthesize_concept`) is cheap; a genuinely novel concept pays for its first summary slice and its first synthesis. The `summarize_concepts` worker switches to **batch-only mode** once a concept has one complete slice (Scope E §5: `hasComplete && len(uncovered) < batchSize` → `PairsSkippedNoDelta++`), so summary cost is amortized in batches of `BatchSize=20` rather than paid per fact. The `deduplicate_facts` worker's `mergeSources` (Scope E §4) means a fact restated across 20 sources ends as one fact with 20 `fact_sources` rows — citation density accumulates, not fact count.

**Phase 2 — research.** An agent navigates the graph built in Phase 1 via the MCP surface: `searchFacts` for fact-level retrieval, `searchConcepts`/`getConcept`/`getConceptSummaries` for concept-level reading, `getRelatedConcepts` for structural-graph traversal, `searchFacts(concepts=[X,Y])` for N-ary concept intersection (Scope E §10). Investigations collect sources; reports are written and submitted to auto-annotation, which sentence-embeds the report against the fact pool and persists `report_annotations` with posture labels (Scope E §8).

**Contrast with one-shot RAG.** A one-shot RAG system (LlamaIndex `VectorStoreIndex` with `SentenceSplitter`) builds an index once and queries it many times. The index does not compound: each query is the same cost, and the index has no internal structure beyond chunk vectors — no concepts, no relations matview, no syntheses. There is no "the knowledge base got denser" property. OKT's `searchFacts` can be used as a one-shot RAG backend (it is the lowest-information-level tool), but OKT also offers concept-level and graph-level navigation that one-shot RAG does not.

**Contrast with end-to-end agents.** AI Scientist v1/v2 generates a complete paper in one pass; its intermediate state (idea drafts, code iterations, write-up drafts) is not structured as a queryable graph that survives the run. There is no "the substrate persisted and got denser across papers" property — each run starts from scratch (modulo the v1 "growing archive" feature, which is a flat list of prior ideas, not a fact graph).

**The paradigm claim (attribution-grounded):** OKT's two-phase separation positions it as a **substrate on which** either one-shot RAG-style retrieval (`searchFacts`) or end-to-end agent-style research (the full MCP surface) can be performed, with the substrate persisting and compounding across both. This is a structural claim about layering, not a performance claim.

**Agent hypothesis (untested, not stated by any source):** the three paradigms may be complements at different scales rather than substitutes — single query over small corpus → one-shot RAG is sufficient; corpus queried many times → a compounding substrate amortizes its build cost; single end-to-end paper → AI Scientist's tight loop is appropriate; multi-paper programme over a growing corpus → a compounding substrate with an agent on top. This is a scale-contingent complementarity hypothesis, grounded in the observation that one-shot RAG's per-query cost is roughly constant, AI Scientist's per-paper cost is roughly constant ($15, [fact](<fact:177b7c21-7ec9-402b-a45f-98bf2d68ac53>)), and OKT's per-source cost is decreasing in corpus size (cache hits + batch-only summaries). Confirming it would require Experiment 5's compounding-vs-one-shot-RAG curve plus an AI-Scientist-on-OKT variant — outside this document's six-experiment scope.

---

## Part 2 — Proposed Experimental Methodology (Run Locally)

Six experiments. For each: (a) Hypothesis, (b) Systems compared, (c) Dataset, (d) Metric, (e) Protocol naming actual OKT MCP tools/endpoints and baseline libraries, (f) Confounders with symmetric controls, (g) Parallel-scenario outcomes stated in advance.

**General control (applies to all six experiments).** Fix the embedding model (`google/gemini-embedding-2`, 3072-dim, Scope E §4) and the extraction/synthesis LLM (`google/gemma-4-31b-it` for extraction and concept-extraction; `deepseek/deepseek-v4-flash:turbo` for synthesis, Scope E §5) across **both** OKT and any baseline that uses an LLM. This is the single most important control: if OKT wins because its embedding is better, that is an embedding-vendor result, not an architecture result. The baseline must use the same embedding where it embeds anything, and the same LLM where it calls an LLM.

### 7. Experiment 1 — Fact-level vs chunk-level retrieval precision

**(a) Hypothesis.** Atomic-fact retrieval (OKT `searchFacts`) yields higher precision@k than chunk-level retrieval (LlamaIndex `VectorStoreIndex` with `SentenceSplitter(chunk_size=512, chunk_overlap=50)`) for specific factual questions, because a fact is a self-contained semantic unit while a chunk mixes the target fact with adjacent prose.

**(b) Systems compared.** OKT `searchFacts` MCP tool (Scope E §10, full-text + Qdrant hybrid via `websearch_to_tsquery` + vector search) vs LlamaIndex `VectorStoreIndex` over the same PDFs with the fixed embedding.

**(c) Dataset.** 50–100 scientific papers (the OKT default repo's existing corpus, or a fresh 50-paper ultra-processed-foods corpus), plus 50 held-out factual questions with a gold answer = a specific atomic fact present in one of the papers. Questions are single-fact ("What daily intake of X was associated with outcome Y in study Z?"), not synthesis questions.

**(d) Metric.** Precision@1, @3, @5, @10; citation precision (for OKT: does `getFact(factId)` return a `fact_sources` row whose source URL matches the gold paper? for LlamaIndex: does the retrieved chunk's source file match the gold paper?).

**(e) Protocol.**
1. Ingest the 50–100 papers via `fetchAndProcessSource(url=...)` (Scope E §9), one MCP call per paper, `Process: true` (default).
2. Drain: poll `getSourceTasks(repository, verbose=false, limit=200)` per the drain protocol (Scope E §9), paging every `next_cursor` accumulating `pending_count`; declare drained only when the final page has `next_cursor` empty AND `pending_count=0`; verify `synthesize_concept` jobs completed via `getSourceTasks(byKind=true)`.
3. Build a LlamaIndex `VectorStoreIndex` over the same PDFs with `SentenceSplitter(chunk_size=512, chunk_overlap=50)` and the fixed embedding (`google/gemini-embedding-2` via OpenRouter).
4. For each of 50 questions: call `searchFacts(repository, query=q, limit=10)`; record top-10 fact IDs; call `getFact` on each to extract source URLs; score precision@k and citation precision. Simultaneously query the LlamaIndex index for top-10 chunks; score the same metrics against the gold paper.
5. Report per-question and aggregate precision@k for both systems.

**(f) Confounders with symmetric controls.**
- OKT extraction quality: audit a 50-fact random sample for self-containedness and correctness (use the audit to flag any OKT loss attributable to bad extraction rather than bad retrieval).
- LlamaIndex chunk_size: sweep {256, 512, 1024} and report the best-performing setting for LlamaIndex (do not fix at 512 if 256 wins).
- Same embedding both sides (general control).
- Question difficulty: stratify by single-fact (gold answer is one atomic fact) vs multi-sentence-context (gold answer requires two adjacent facts); report per-stratum.

**(g) Parallel-scenario outcomes (stated in advance).**
- **OKT-favourable:** precision@1 ≫ LlamaIndex on single-fact questions; citation precision = 1.0 (because `getFact` always returns the source URL via `fact_sources`).
- **Baseline-favourable:** LlamaIndex wins recall on multi-sentence-context questions (because the surrounding prose in a chunk provides context the bare atomic fact lacks).

**Honest prediction.** OKT wins precision@1 on single-fact questions; LlamaIndex wins recall on synthesis questions. This is a design prediction, not a performance claim — atomic-fact retrieval is engineered for precision on single-fact queries, chunk retrieval for recall on context-bearing queries.

### 8. Experiment 2 — Hallucination detection via auto-annotation vs SelfCheckGPT/FActScore

**(a) Hypothesis.** OKT `annotate_report` (Scope E §8: `similarity_threshold=0.7` config default / `0.84` code fallback, `lexical_similarity_floor=0.6`, posture on, `google/gemma-4-31b-it` classifier, batch 8 concurrent 4) detects inserted hallucinations at higher F1 than SelfCheckGPT consistency checking and FActScore atomic attribution, *for claims in the OKT fact pool's domain*, because OKT checks against a curated fact pool built from the same domain.

**(b) Systems compared.** OKT `createReport` + `getReport` (the auto-annotation pipeline) vs SelfCheckGPT (sample N=5 at T=1.0, the zero-resource baseline) vs FActScore (`pip install factscore`, Wikipedia as the knowledge source).

**(c) Dataset.** 20 human-written reports ~1000 words each in the OKT default repo's domain; 10 with 5 deliberate hallucinated sentences each (claims absent from the corpus but plausible), 10 clean. Sentence-level ground truth (which sentences are hallucinated) is hand-labeled.

**(d) Metric.** Per-sentence precision/recall/F1 for hallucination detection. OKT detection rule: a sentence is flagged hallucinated if it has **zero** `report_annotations` (no fact in the pool matched it) **OR** its highest-score annotation has `posture=contradicts`. SelfCheckGPT rule: a sentence is flagged if inter-sample consistency is below threshold. FActScore rule: a sentence is flagged if any of its atomic facts is not supported by Wikipedia.

**(e) Protocol.**
1. Confirm the corpus has been ingested and the pool drained (same drain protocol as Experiment 1).
2. For each of the 20 reports: call `createReport(repository, title, text=report_body)`. This enqueues an `annotate_report` job (Scope E §8). Poll `getReportTasks(repository, reportId, verbose=false)` until `complete=true` and `pending_count=0` (Scope E §8 drain). Call `getReport(repository, reportId)` to read `report_annotations` (each carrying `sentence_index`, `fact_id`, `score`, `posture`).
3. Run SelfCheckGPT on the same 20 reports: sample N=5 responses per report at T=1.0, compute per-sentence consistency, flag below-threshold sentences.
4. Run FActScore on the same 20 reports: decompose each into atomic facts, check each against Wikipedia (and a second run against Wikipedia+Google for the web-grounded variant).
5. Score all three against the sentence-level ground truth; report per-sentence P/R/F1.

**(f) Confounders with symmetric controls.**
- OKT `similarity_threshold` sweep {0.78, 0.82, 0.84, 0.88}; report the best OKT setting (do not fix at 0.7 if a higher threshold wins).
- SelfCheckGPT N sweep {3, 5, 10}; report best N.
- FActScore with both Wikipedia-only and Wikipedia+Google; report both.
- OKT posture classifier ON vs OFF (the `postureClassifier.enabled` flag, Scope E §8) — a clean ablation of the posture step.
- Out-of-corpus claims: deliberately include 5 hallucinated sentences whose content is *outside* the OKT pool's domain (e.g., a historical fact in a nutrition repo) to test the out-of-domain failure mode predicted in §5.

**(g) Parallel-scenario outcomes.**
- **OKT-favourable:** F1 > both baselines on in-domain hallucinations; high precision on `contradicts`-flagged sentences; high recall on uncited sentences (zero annotations).
- **Baseline-favourable:** SelfCheckGPT and FActScore win on out-of-corpus claims (because OKT's pool does not cover them, so a true claim outside the pool is flagged as "uncited" = false positive, and a hallucinated claim outside the pool is also flagged = the flag is uninformative); OKT false-positives on clean sentences the pool happens not to cover.

**Honest prediction.** OKT wins in-domain, loses out-of-domain. The structural reason is the curated-pool design choice (§5): a curated pool is high-precision inside its scope and blind outside it, where SelfCheckGPT's sampling-variance approach is domain-agnostic but blind to consistently-made false claims. The two are complements on different failure modes, not substitutes on the same one.

### 9. Experiment 3 — Dedup quality vs naive exact-match / embedding-cluster dedup

**(a) Hypothesis.** OKT's stable-wins + `mergeSources` dedup (Scope E §4, threshold 0.94 cosine, stable-wins, `mergeSources` preserves non-overlapping `fact_references` and relinks `fact_concepts`) preserves more citation density on survivors than exact-hash dedup and DBSCAN-cluster dedup, without excessive false merges.

**(b) Systems compared.** OKT `deduplicate_facts` (the live worker) vs exact-hash dedup (SHA-256 of normalized fact text) vs scikit-learn DBSCAN on the fact embeddings (eps = OKT threshold, same embeddings).

**(c) Dataset.** 20 review articles + 20 primary studies on the same topic (reviews restate findings in different wording — the canonical dedup stress test).

**(d) Metric.** (i) Distinct facts after dedup; (ii) citation density on survivors (avg `fact_sources` count, avg `fact_references` count); (iii) manual audit of a 100-pair random sample: false-merge rate (two genuinely different facts collapsed) and false-keep rate (two genuinely duplicate facts kept separate).

**(e) Protocol.**
1. The existing scaffolding `scripts/experiments/dedup-threshold-sweep/dedup_threshold_sweep.py` connects directly to Qdrant (6333 REST) and Postgres (5432), replays the `deduplicate_facts` nearest-neighbor search for every embedded fact of a single investigation, and sweeps thresholds 0.80–0.98 (Scope E §12). It is read-only and reports two views: any-neighbor (upper bound) and cross-source-only (matches the live worker). Existing report: `reports/dedup_sweep_shadow_fleet.html` (139 sources, 4,427 embedded text-stable facts, `gemini-embedding-2`, 12.5s sweep).
2. Extend this scaffolding to run three strategies — OKT stable-wins, exact-hash, DBSCAN — on the same fact set, and compute (i)–(iii) for each.
3. For the 100-pair manual audit: two labelers independently classify each pair as "same fact" / "different fact"; report inter-annotator agreement (Cohen's κ); disagreements adjudicated by a third labeler.

**(f) Confounders with symmetric controls.**
- OKT threshold sweep {0.86, 0.90, 0.94}; report best.
- DBSCAN shares the OKT threshold as `eps` (so the threshold confounder is symmetric — both systems at the same cosine cutoff).
- 2 labelers + inter-annotator agreement (the manual audit is the only subjective step; everything else is mechanical).
- Same embedding both sides (general control).

**(g) Parallel-scenario outcomes.**
- **OKT-favourable:** citation density higher via `mergeSources` (mechanical consequence — `mergeSources` unions `fact_sources` and preserves non-overlapping `fact_references`, Scope E §4, so density strictly cannot decrease on a true merge); false-merge rate lower than DBSCAN at matched threshold (OKT's stable-wins ordering is more conservative than DBSCAN's symmetric clustering).
- **Baseline-favourable:** DBSCAN achieves comparable citation density at lower compute (DBSCAN is one pass over the embedding matrix; OKT's worker is per-fact Qdrant queries); OKT false-merge rate non-trivial at the default 0.94 threshold (the threshold is a global setting; some fact pairs at 0.94 cosine are genuinely different).

**Honest prediction.** OKT wins citation density — this is a mechanical consequence of `mergeSources`, not an empirical question. False-merge rate is genuinely uncertain; the threshold-sweep scaffolding will show the precision-recall curve, but the 100-pair audit is the only way to see whether 0.94 is well-calibrated for the test domain. A domain where 0.94 is too aggressive (e.g., a domain with many near-identical numerical facts) would favor exact-hash; a domain with heavy paraphrasing would favor OKT.

### 10. Experiment 4 — Emergent concept quality vs top-down ontology mapping

**(a) Hypothesis.** OKT's emergent concept set covers more of the entities a human annotator identifies than top-down DBpedia Spotlight, at the cost of higher fragmentation (more system-IDs per gold entity).

**(b) Systems compared.** OKT `searchConcepts(limit=200)` (the emergent concept set, Scope E §2) vs DBpedia Spotlight (confidence 0.5) vs a Wikidata linker (mGENRE or BLINK).

**(c) Dataset.** The 50–100-paper corpus from Experiment 1, plus 30 hand-built shared-alias test cases ("N" → Nitrogen / Neutron / Nano / Number; "Washington" → person / city / state; "Mercury" → element / planet / god / brand).

**(d) Metric.** (i) Coverage (% of human-annotator gold entities found by the system); (ii) fragmentation (avg system-IDs per gold entity; 1 = ideal); (iii) disambiguation accuracy on the 30 shared-alias cases (does the system assign the right context?).

**(e) Protocol.**
1. The existing scaffolding `scripts/experiments/disambiguation_benchmark.py` (1238 lines, Scope E §12) tests whether a reduced context list (the 88-category `manual_select.json`) preserves the disambiguation power of the original 789-label `dbpedia_l3.json`. It crafts ambiguous facts, sends each to `google/gemma-4-31b-it` via OpenRouter twice (original vs candidate list), and compares the (concept, context) pairs. Output: `disambiguation_benchmark.json`.
2. Extend this scaffolding to run three systems — OKT concept extraction (the live `extract_concepts` worker over the corpus), DBpedia Spotlight (confidence 0.5), and a Wikidata linker — on the same corpus, and compute (i)–(iii).
3. For OKT disambiguation specifically: the per-fact `ResolveAliasMatchForFact` helper (Scope E §2) uses cosine tie-break between the fact's Qdrant vector and each candidate concept's vector when an alias is shared. The 30 shared-alias test cases directly exercise this path.
4. The human annotator gold entity list is built once, by one annotator, for all three systems (symmetric annotation).

**(f) Confounders with symmetric controls.**
- OKT context vocabulary choice: run with DBpedia L3 (789), `manual_select.json` (88), and the scientific preset (~30); report all three.
- Spotlight confidence sweep {0.3, 0.5, 0.7}; report best.
- Same human annotator for all three systems (no cross-annotator label noise).
- The 30 shared-alias cases are deliberately adversarial — report them separately from the corpus-wide coverage/fragmentation numbers.

**(g) Parallel-scenario outcomes.**
- **OKT-favourable:** coverage > Spotlight (the emergent set includes corpus-specific entities — e.g., a specific dataset or method name — that no top-down ontology has); disambiguation > Spotlight on shared-alias cases (because per-fact embedding tie-break uses the *fact's* context, not just the alias string).
- **Baseline-favourable:** Spotlight + Wikidata win disambiguation via pre-built ontology + alias maps (Wikidata's alias maps are human-curated and encyclopedic); OKT fragmentation higher (the same entity surfaced as `Biomolecule` in one paper and `chemical compound` in another creates two OKT concept rows, where Spotlight would map both to one DBpedia entity).

**Honest prediction.** OKT wins coverage (mechanical — emergent extraction cannot miss a corpus-specific entity that a top-down ontology does not have); OKT loses fragmentation (mechanical — the `(canonical_name, context)` identity is more granular than a single DBpedia URI). Disambiguation is genuinely uncertain — OKT's per-fact embedding tie-break is a clever mechanism, but Wikidata's curated alias maps are an entirely different and very strong mechanism. The 30-case shared-alias test is the discriminating experiment.

### 11. Experiment 5 — Compounding vs one-shot RAG over a growing corpus

**(a) Hypothesis.** OKT's marginal LLM cost per new source decreases as the corpus grows (cache hits on existing concepts, `BatchSize=20`-gated summary skip), while one-shot RAG's marginal cost is roughly constant, and OKT answer quality on a fixed test set improves faster per marginal dollar.

**(b) Systems compared.** OKT (ingest one source at a time, measure after each) vs LlamaIndex re-build-from-scratch after each source vs LlamaIndex incremental-insert after each source.

**(c) Dataset.** The 50–100-paper corpus from Experiment 1, in a fixed random order, plus a fixed set of 20 questions (mixed single-fact and synthesis).

**(d) Metric per newly-ingested source.** (i) LLM tokens consumed (OKT: read from `okt_system.ai_usage`; LlamaIndex: embedding API call count + tokens); (ii) wall-clock drain time (OKT: from `fetchAndProcessSource` return to `getSourceTasks` complete; LlamaIndex: from insert call to index-ready); (iii) precision@5 on the 20 questions; (iv) OKT graph density (avg `shared_fact_count` from the `concept_relations` matview, Scope E §6); (v) OKT summary freshness (fraction of concepts whose `concept_summaries.is_complete=true` and whose `concept_syntheses` row exists).

**(e) Protocol.**
1. Fix a random order of the 50–100 papers.
2. For source `i` in 1..N:
   a. OKT: call `fetchAndProcessSource(url=paper_i_url)`. Run the full drain protocol (Scope E §9). Record `okt_system.ai_usage` delta, wall-clock, and graph density after drain. Run the 20 questions via `searchFacts`; record precision@5.
   b. LlamaIndex rebuild: insert paper `i` into a fresh `VectorStoreIndex` (re-build from scratch each time). Record embedding API cost, wall-clock. Run the 20 questions; record precision@5.
   c. LlamaIndex incremental: insert paper `i` into the existing `VectorStoreIndex` (incremental insert, no rebuild). Record cost, wall-clock. Run the 20 questions; record precision@5.
3. Plot per-source cost, cumulative cost, precision@5, and (for OKT) graph density and summary freshness, all as functions of `i`.

**(f) Confounders with symmetric controls.**
- OKT `BatchSize=20` makes cost bursty (a single source that triggers a batch boundary pays for 20 facts' worth of summary; the next 19 sources pay nothing for that concept's summary). Report a **rolling 20-source average** of tokens-per-source, not the per-source value, to smooth the batch boundary.
- LlamaIndex gets two baselines (rebuild + incremental) — the rebuild is the worst case, the incremental is the fair case. Compare OKT to the *incremental* baseline for cost.
- Same embedding, same order, same questions (general + design controls).

**(g) Parallel-scenario outcomes.**
- **OKT-favourable:** rolling-average tokens-per-source trends downward as `i` grows (cache hits + batch-only summaries); precision@5 rises faster per cumulative token than either LlamaIndex baseline; graph density rises monotonically (mechanical — `shared_fact_count` is non-decreasing as facts accumulate).
- **Baseline-favourable:** LlamaIndex-incremental also has roughly constant per-source cost (embedding one more document) and lower per-source cost than OKT (no LLM extraction step); precision@5 is comparable (chunk retrieval on a growing corpus is well-studied and works).

**Honest prediction.** OKT's per-source cost will **not** cleanly trend downward enough to beat LlamaIndex-incremental on cost alone — OKT pays an LLM extraction cost per source that LlamaIndex does not, and that cost is roughly constant per source. The real question is whether precision@5 rises *faster per cumulative token* — i.e., whether the structured graph produces more answer-quality per dollar than the flat index. This is the experiment's discriminating question; the cost curve alone is not.

### 12. Experiment 6 — GraphRAG-style global QA vs OKT concept synthesis

**(a) Hypothesis.** OKT's `getConcept` (synthesis) + `getRelatedConcepts` + `searchFacts(concepts=[X,Y])` yields comparable comprehensiveness and diversity to Microsoft GraphRAG's community-summary approach, at lower per-query token cost, because OKT's syntheses are pre-built during ingestion while GraphRAG builds community summaries at indexing time. **Correction flagged:** GraphRAG in fact builds community summaries at *indexing* time, not query time (Scope D §5, [fact](<fact:5d1efbd1-2781-44de-abb4-1c32891f9425>)) — so the per-query token cost comparison is "OKT reads pre-built syntheses vs GraphRAG reads pre-built community summaries," both indexing-time-built. The remaining cost difference is in *what* each reads (entity-shaped syntheses vs cluster-shaped community summaries) and *how many* (one synthesis per concept vs one summary per community). The honest hypothesis is therefore about comprehensiveness/diversity/grounding, not indexing-vs-query-time cost.

**(b) Systems compared.** OKT (`searchConcepts` → `getConcept` → `getRelatedConcepts` → `searchFacts(concepts=[X,Y])`) vs Microsoft GraphRAG (`graphrag query --method global`).

**(c) Dataset.** The 50–100-paper corpus from Experiment 1, plus 20 global/theme questions ("What are the main mechanisms by which X affects Y across this literature?" — questions that require synthesizing across many papers, not retrieving one fact).

**(d) Metric.** GraphRAG's own comprehensiveness + diversity metrics (LLM-as-judge pairwise comparison between OKT and GraphRAG answers) + token cost per question (OKT: `ai_usage` delta; GraphRAG: run logs) + grounding (human annotator: does the answer cite facts that exist in the corpus? for OKT this is checkable via the `[text](<fact:fact_id>)` citations in syntheses; for GraphRAG this requires tracing community summaries back to source entities).

**(e) Protocol.**
1. Ingest the corpus into OKT and run the full drain protocol (Scope E §9). Critically, confirm `synthesize_concept` jobs are completed via `getSourceTasks(byKind=true)` — OKT's syntheses only exist when those jobs finalize, and global QA depends on them.
2. Build a GraphRAG index over the same corpus (`graphrag index`). Report GraphRAG indexing cost (tokens + wall-clock) separately — this is the apples-to-apples indexing comparison against OKT's Phase 1 cost.
3. For each of 20 global questions:
   a. OKT: run the navigation pattern `searchConcepts(query=question_terms) → getConcept(top_concept) → getRelatedConcepts(top_concept) → searchFacts(concepts=[top_concept, related_concept])`; assemble an answer from the syntheses + retrieved facts.
   b. GraphRAG: run `graphrag query --method global --query question`.
   c. Record `ai_usage` delta (OKT) and run logs (GraphRAG) for token cost.
4. LLM-as-judge pairwise: present OKT and GraphRAG answers (blind, randomized order) to a judge LLM (the same model both sides) for comprehensiveness and diversity.
5. Human annotator: for each answer, check whether cited facts/entities exist in the corpus (grounding).

**(f) Confounders with symmetric controls.**
- OKT syntheses only exist when `synthesize_concept` jobs complete — confirm via `getSourceTasks(byKind=true)` before running queries; if not drained, the comparison is unfair to OKT.
- GraphRAG indexing cost reported separately from per-query cost (the indexing cost is the apples-to-apples comparison against OKT's Phase 1; the per-query cost is the apples-to-apples comparison against OKT's Phase 2).
- Same LLM for the judge and for any LLM calls in either system (general control).
- Question-type stratification: 10 "single-theme" questions (one concept cluster) and 10 "cross-theme" questions (multiple clusters); report per-stratum.

**(g) Parallel-scenario outcomes.**
- **OKT-favourable:** ≥ GraphRAG on comprehensiveness/diversity (entity-shaped syntheses with related-concept traversal cover the same ground as cluster-shaped community summaries); lower per-query token cost (reading pre-built syntheses vs running a global query that re-summarizes); higher grounding (mechanical — OKT syntheses carry `[text](<fact:fact_id>)` citations, GraphRAG community summaries trace to entities not facts).
- **Baseline-favourable:** GraphRAG wins comprehensiveness/diversity for fresh corpora (community detection finds thematic clusters that emergent entity extraction may not surface as concepts); GraphRAG indexing cost lower than OKT Phase 1 (GraphRAG's two-stage LLM pipeline is simpler than OKT's 7-stage pipeline).

**Honest prediction.** OKT most likely **loses** on comprehensiveness/diversity for fresh corpora — GraphRAG is purpose-built for global sensemaking, and its community-detection thematic clustering is a structural advantage for cross-cutting theme questions that OKT's entity-shaped syntheses are not designed for. OKT should win **grounding** (mechanical citation advantage — every claim in an OKT synthesis traces to a `fact_id`, every `fact_id` traces to a source URL via `getFact`). Per-query cost is uncertain — both systems read pre-built structures, so the cost difference is in the read pattern, not in indexing-vs-query. Question-type-dependent: single-theme questions favor OKT (one `getConcept` call); cross-theme questions favor GraphRAG (one community-summary read spans multiple themes).

### 13. Significance

The six experiments ask "which architectural commitment of OKT pays off against which baseline, and at what cost," not "is OKT better." The honest predictions across the six experiments reflect a design expecting OKT to win on **fact-level precision** (Experiment 1), **citation density** (Experiment 3 — mechanical), and **grounding** (Experiment 6 — mechanical citation advantage); to be **competitive on compounding cost** (Experiment 5 — empirical, with the real question being quality-per-dollar not cost-per-source); and to be **disadvantaged on cross-cutting theme QA** (Experiment 6) and **out-of-corpus hallucination detection** (Experiment 2). Disambiguation (Experiment 4) is genuinely uncertain — the per-fact embedding tie-break is a clever mechanism, but Wikidata's curated alias maps are a very strong baseline.

The broader significance is that OKT's contribution is a **paradigm shift in what the intermediate representation IS** — from chunks (one-shot RAG) or ephemeral agent state (AI Scientist v1/v2) to a persistent, compounding, fact-grounded graph that an agent navigates at multiple information levels (fact, concept, related-concept, N-ary intersection) and that audits the agent's output via auto-annotation. Whether this paradigm shift produces better research outcomes per dollar is exactly the empirical question the six experiments settle. The parallel-scenario framing ensures that **either** outcome is reportable as a finding: an OKT win on a given experiment is a finding that the architectural commitment pays off there; a baseline win is a finding that it does not. No experiment is designed to be un-falsifiable in either direction.

### 14. Note on sources fetched

**Found in the OKT fact pool and cited above:**
- Pan et al. 2024, "Unifying Large Language Models and Knowledge Graphs: A Roadmap," IEEE TKDE 36(7):3580–3599 — authorship and publication metadata ([fact](<fact:f7555e71-38f9-4d78-9766-1bfc680aa08a>)), three-framework taxonomy ([fact](<fact:3917c17c-6f66-4edf-b736-06fae0bb824f>)).
- Bian, "LLM-empowered knowledge graph construction: A survey" — submission metadata ([fact](<fact:1e5e6182-a108-451d-bbc9-b4b1b7f5aef3>)), three-layered pipeline ([fact](<fact:8372702a-e340-4ad5-afcc-d8ad0208a51e>)), schema-based vs schema-free ([fact](<fact:0e30ac79-f15c-484e-a5f1-e6ad06626781>)), future directions ([fact](<fact:906ff09d-a16f-41aa-93ae-ae9f3b406f96>)).
- Abu-Rasheed, Weber, Fathi, "Knowledge Graphs as Context Sources for LLM-Based Explanations of Learning Recommendations," IEEE EDUCON 2024 ([fact](<fact:b72f9720-0c01-43e2-a406-969b7d6d5dd8>)).
- AI Scientist v1 — authorship and arXiv ID ([fact](<fact:e62f8221-13f0-46fd-8097-84860c2ade21>)), Sakana/Oxford/UBC collaboration ([fact](<fact:919875d4-e470-4e64-9659-cde3eab9d0f7>)), $15/paper cost ([fact](<fact:177b7c21-7ec9-402b-a45f-98bf2d68ac53>)), "Weak Accept" claim ([fact](<fact:7a7d4d69-a1c6-4596-8c34-4824233c6615>)), incorrect-implementation limitation ([fact](<fact:98b45eaa-54f8-4d87-853a-1b2d476a1aa8>)), magnitude-comparison pathology ([fact](<fact:d50ac75c-353e-4cc9-a7cf-725656cec118>)), self-modification incidents ([fact](<fact:6c845180-d91c-4a81-80c6-973f4544a312>)) ([fact](<fact:43c15187-f0da-4d5d-b3bb-34c724f22f7c>)).
- AI Scientist v2 — Yamada co-first-author role ([fact](<fact:f1ed7a7e-e34b-49c3-b51b-50cc34847c49>)), ICLR ICBINB submission and acceptance ([fact](<fact:3e740e33-77fe-4d67-9a26-66f8c087b47d>)), "first instance of a fully AI-generated paper successfully navigating a peer review" ([fact](<fact:c668b63b-409f-4818-af44-2628a54194a3>)), citation hallucination acknowledgement ([fact](<fact:eecd054d-4d61-4bb0-b25e-d04666c3edaa>)). The v2 concept's full synthesis (with its own parallel-scenario framing of the peer-review outcome) was read via `getConcept` and informed the framing in §4.
- FActScore — authorship and arXiv ID ([fact](<fact:020f539a-686c-4d04-9f80-c32b8a474ed2>)) ([fact](<fact:d841105a-b89f-4713-8a9d-ef2044941507>)) ([fact](<fact:00feb820-dff8-4023-abfd-c055062959f7>)), motivation ([fact](<fact:136446d9-6efb-4a9c-92e1-0dbe199a4bd6>)).
- ALCE / Gao et al. 2023 — authorship and arXiv ID ([fact](<fact:6087b0e8-d6ae-4ddb-802f-04789a274b8b>)) ([fact](<fact:aa43dc24-dfb2-4e3d-941b-e744c3dc57b5>)) ([fact](<fact:9d044a59-6b40-4c88-9eb2-cb22c31f94c9>)), ALCE as first citation-eval benchmark ([fact](<fact:1ff1cc64-0249-4b10-9727-d606b638982e>)), automatic metrics ([fact](<fact:f3ef6825-dc0f-4f8c-8e6d-3424d6f38a40>)).
- SelfCheckGPT — black-box zero-resource scheme ([fact](<fact:79c2fdf1-e857-4e85-861e-0c5bee058e55>)) ([fact](<fact:288cedd7-802f-4913-934a-ca0e61e1e739>)), consistency premise ([fact](<fact:792f2c72-d324-4207-a2c8-91dfb4470903>)) ([fact](<fact:71aef449-0e66-470a-9551-b89406ba6443>)) ([fact](<fact:bd185650-ea65-4626-a106-f2fee378a4a2>)) ([fact](<fact:a3acd7b3-8e96-470b-a4a0-5d0017ed4ce0>)) ([fact](<fact:4d3c46b3-7454-4655-948d-7cc97f9d8757>)).
- RAGTruth — corpus description ([fact](<fact:46934bc3-9cff-4e30-b70a-1b670ce6a6cb>)), span-level labels ([fact](<fact:6a0c2c6a-4383-4709-ad0e-746c97de2307>)) ([fact](<fact:ace10e83-919e-4fbd-827f-4956060a0468>)), statistics ([fact](<fact:d1ed08b7-df87-4c46-82d3-9fb1a8eec4f4>)) ([fact](<fact:3532f9e5-457e-4c24-87f5-adb4a3a72a04>)) ([fact](<fact:c272d59c-060a-4254-8cb7-18ed6c6ce1ed>)) ([fact](<fact:e1f1973e-38f1-4915-874a-2c358dfa0413>)) ([fact](<fact:3a087c2e-bde2-41ef-925e-57b986ef3a27>)) ([fact](<fact:9876e414-c56c-48f2-b6f3-7bcf72329c71>)) ([fact](<fact:6281442c-ca56-4415-a7f1-78ba272aa137>)), fine-tuned-Llama-beats-prompted-GPT-4 claim ([fact](<fact:02f2e394-e178-4e94-bfad-3d49bd22f9f3>)) ([fact](<fact:5597ec05-fc19-41cc-a519-65017f3eadb8>)), fine-tuning details ([fact](<fact:95abbfe2-0ab6-4b32-87ee-a1b262b080c8>)) ([fact](<fact:6432dcc3-1198-4448-8f9d-10d3df0d6434>)) ([fact](<fact:909b468e-9591-4247-b2b3-c7fc4d6ca64b>)), per-task hallucination rates ([fact](<fact:50db6d96-6ef8-4089-9aa1-5c438d71828d>)) ([fact](<fact:3fc63762-ac0d-4b19-a97e-b033ed2be2dc>)) ([fact](<fact:61bd8e57-5390-4f4e-9605-bc94394b414f>)) ([fact](<fact:4fbac381-c8d3-4f71-8df8-eafbf0cb81e4>)).
- Self-RAG — authorship and arXiv ID ([fact](<fact:3703996e-09b3-4f67-b798-bd6c3cd0d09e>)), reflection-token framework ([fact](<fact:71346ff2-33af-42df-acc4-9587c9a13b52>)) ([fact](<fact:03bed1aa-e350-480a-87d6-950d525fce2c>)).
- GraphRAG — authorship ([fact](<fact:ca72ea19-2aa2-4e4f-956e-9dbfbd5fa6d1>)), two-stage graph index ([fact](<fact:5d1efbd1-2781-44de-abb4-1c32891f9425>)).
- RAGAs — authorship and venue ([fact](<fact:c1833067-a037-4fa1-ac4b-d9eb11c8f146>)).

**Not found in the OKT fact pool (flagged — not paraphrased from memory):**
- FActScore's reported 58% ChatGPT factuality number — the fact pool contains the paper's authorship, publication, and motivation but not the specific 58% headline figure. Do not state this number as fact; it must be fetched and verified before inclusion.
- ALCE's reported "50% lack complete support" figure — same situation; the pool has the paper but not the specific headline metric.
- RARR (Gao et al., retrieval-augmented attribution) — searched, not in the pool. Referenced in §5 only as the named cousin of OKT's auto-annotation; not cited as a fact.
- "Wang survey autonomous" / a specific Wang-author survey on autonomous agents — searched, not found; the AI Scientist v1/v2 papers stand in for the autonomous-agent literature.
- Specific numerical results from AI Scientist v1's evaluation beyond "$15/paper" and "Weak Accept" — the pool has the headline claims but not the per-paper evaluation table.

Any future revision of this document that states one of these numbers must first `fetchAndProcessSource` the relevant paper into the repo and drain the pipeline before citing the resulting fact.