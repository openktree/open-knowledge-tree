# Atomic Facts and Emergent Concept Graphs: A Fact-Based Architecture for Retrieval-Augmented Generation[1:rel][2:rel]

## Abstract

This paper presents two interlocking contributions from the Open Knowledge Tree (OKT) system. First, we argue — and present benchmark evidence — that decomposing documents into *atomic, self-contained, source-attributed facts* is an effective retrieval granularity for retrieval-augmented generation (RAG). A MultiHop-RAG benchmark run (n=2556 questions, 609 news articles) on the OKT atomic-fact index yielded accuracy of 0.757 with hallucination held to a narrow 7.7–11.1% band across *all* retrieval configurations, including a naive direct-retrieval baseline — a pattern suggesting hallucination control is a structural property of the chunking substrate rather than of any query-extraction step layered above it [Experiment: MultiHop-RAG, n=2556]. We compare this design against Dense X Retrieval's "proposition" unit and weigh both against a late-chunking counter-argument, applying symmetric skepticism to all three positions, and report a matched-conditions head-to-head experiment (§3.7) that finds atomic facts win on accuracy (non-overlapping CIs) while late chunking wins on hallucination control. Second, we describe an autonomous concept-graph construction paradigm in which an LLM extracts *concepts* (as tags on atomic facts) and concept-to-concept relations are never extracted at all — they are *computed* as the count of facts in which two concepts co-occur, forming a bipartite fact↔concept graph whose monopartite projection is a weighted co-occurrence network.[3:supp] We contrast this with triplet-extraction knowledge graphs (REBEL, OpenIE, EDC, AEVS, GKE) and top-down ontology mapping (DBpedia, Wikidata, YAGO), noting that the concept graph is an organization structure (a navigational index over facts), not a knowledge base (which stores knowledge in relations), and presenting each comparison as a trade-off rather than a clear win. We identify five novel design-level properties that a fact-based database enables (co-occurrence relations, source_count confidence, sentence-level provenance, multi-context concept groups, compounding), each with a stated failure mode. Finally, we *execute* a structural-experiment protocol (BiCM null-model validation, seven graph-property measurements across four W thresholds, and five failure-mode audits) on the `multihoprag` repository's concept graph (18,241 concepts, 76,233 co-occurrence relations), confirming that the emergent graph has usable structure for navigation and synthesis while quantifying the failure rates, and *replicate* the protocol on the `default` repository's concept graph (267,754 concepts, 2,033,656 co-occurrence relations), confirming that the structural findings (lognormal degree, strong modularity, small-world navigability, BiCM degree-confounding of raw weights) hold at 13× scale while the extraction-quality failure rates are corpus-sensitive. The paper is deliberately bounded: it covers fact-RAG and autonomous concept-graph building, not the agent layer, report synthesis, or other OKT subsystems.

---

## 1. Introduction

Retrieval-Augmented Generation (RAG) is the standard approach for equipping large language models with up-to-date knowledge, yet its effectiveness hinges on a design choice often treated as plumbing rather than architecture: the granularity at which the corpus is indexed ([D4:supp]). Symmetrically, the knowledge-graph (KG) construction field is organized around a paradigm choice: should relations be explicitly extracted (bottom-up triplet extraction), mapped to a pre-existing ontology (top-down), or — as OKT proposes — computed rather than extracted at all?

This paper weaves two contributions from the OKT system into a single narrative. The first contribution is **Fact-RAG**: the claim that atomic, self-contained, source-attributed facts are the right retrieval unit, supported by a MultiHop-RAG benchmark run on the OKT fact index. The second contribution is **autonomous concept-graph building**: the claim that an LLM can extract *concepts* (not triplet endpoints) from those atomic facts, and that concept-concept relations can *emerge* from co-occurrence — computed by a SQL view rather than extracted by a relation-classification model. The two contributions are not independent modules bolted together; they form a stack: atomic facts are the foundational unit → concepts are extracted from facts → a co-occurrence graph emerges from which facts mention which concepts → the fact-based database exhibits novel properties → and a structural experiment is designed to validate whether the emergent graph has usable structure.

The narrative arc is: **atomic facts as the foundational unit (§3) → concepts extracted from facts (§4) → emergent co-occurrence graph (§4) → novel properties of the fact-based DB (§5) → structural experiment to validate the graph (§6)**.

**What this paper does NOT cover.** The agent layer, two-phase workflows, image processing, GraphRAG comparison, auto-annotation of reports, report synthesis, and AI Scientist comparison are out of scope. Where the sub-syntheses surfaced these, we set them aside. The structural experiment is executed on two corpora (`multihoprag`, a 609-article news benchmark, and `default`, a 2,478-source multi-domain research corpus); the structural findings replicate across both, while the extraction-quality failure rates are corpus-sensitive (§6.5).

A note on evidence posture. The MultiHop-RAG results are *system results*, not facts in the OKT knowledge graph; they are cited as `[Experiment: MultiHop-RAG, n=2556]`. Every claim grounded in the repository's ingested literature carries an inline `<fact:UUID>` link. Where two independent designs reach the same conclusion, we name the convergence; where they conflict, we present both scenarios at full strength and let the reader compare.

---

## 2. Background and Related Work

### 2.1 The retrieval-unit debate

The retrieval unit — the unit in which a corpus is indexed — is, per the Dense X Retrieval authors, "a significant design choice" that "significantly impacts the performance of both retrieval and downstream tasks" ([D2:supp], [D1:supp]). Open-domain QA has historically relied on passage retrieval with sparse models such as BM25/TF-IDF ([D3:supp]), and the Retrieval-Enhanced Transformer conditions generation on "document chunks retrieved from a large corpus" ([D5:supp]). Practitioners "often split text documents into smaller chunks and encode them separately to facilitate retrieval of smaller portions of text" ([D6:supp]) — a practice whose default unit is a fixed-size passage.

The granularity spectrum runs: **document → passage → sentence → proposition → atomic fact**. The fine-grained pole is articulated by Dense X Retrieval (arXiv:2312.06648), which introduced the "proposition" as a retrieval unit, defining propositions as "atomic expressions within text that each encapsulate a distinct factoid and are presented in a concise, self-contained natural language format" ([D7:supp]). The authors report that "indexing a corpus by fine-grained units, such as propositions, significantly outperforms passage-level units in retrieval tasks" ([D8:supp]) and that constructing prompts with fine-grained retrieved units "improves the performance of downstream question answering (QA) tasks given a specific computation budget" ([D10:supp]). The mechanism they posit is an embedding-quality argument: "dense vector-based retrieval systems often perform better with shorter text segments because the semantics are less likely to be over-compressed in the embeddings" ([D9:supp]).[4:supp] PropRAG extends this line, "utilizing context-rich propositions instead of triples" with "an efficient, LLM-free online beam search over proposition paths" ([D11:supp]), achieving "state-of-the-art zero-shot Recall@5 and F1 scores on the 2Wiki, HotpotQA, and MuSiQue datasets" ([D12:supp]), and arguing that triples suffer "context loss" — "context collapse" — which propositions remedy ([D16:supp]).

The counter-argument is **late chunking** (arXiv:2409.04701). It observes that "chunk embeddings created by splitting documents and encoding them separately can lose contextual information from surrounding chunks, resulting in sub-optimal representations" ([D13:supp]). Its remedy: "late chunking … leverages long context embedding models to first embed all tokens of a long text, with chunking applied after the transformer model and just before mean pooling" ([D14:supp]). The authors claim "chunk embeddings produced by late chunking capture full contextual information, leading to superior results across various retrieval tasks" ([D15:rel]), and that the method is generic and training-free ([D17:supp]). These two positions make opposite bets about where semantic fidelity lives: Dense X/PropRAG bet that atomicity wins (over-compression of long passages hurts embeddings); late chunking bets that context wins (atomizing before embedding throws away disambiguating surrounding tokens). Both rest on single-source evidence in this repository (each asserted by the paper that proposed it, with no independent replication surfaced), so the reader should treat them as design hypotheses with internal validation rather than settled findings.

A third line — a controlled clinical RAG comparison — built "four otherwise identical RAG pipelines that differed only in how documents were segmented" (fixed-size, semantic, adaptive, proposition-based) "to allow direct measurement of chunking effects independent of model or retriever bias" ([D18:supp]). The four strategies ranked by mean accuracy: **Adaptive chunking** highest at 2.37 [95% CI 2.10–2.60], F1 0.63 [0.36–0.78] ([D19:supp]); **Proposition chunking** second at 2.07 [1.80–2.33], F1 0.49 [0.25–0.67] ([D24:supp]); **Semantic chunking** at 2.04 [1.77–2.33], F1 0.46 [0.19–0.64] ([D25:supp]); **Basic RAG** (fixed-size) lowest at 1.64 [1.40–1.90], F1 0.21 [0.00–0.39] ([D23:supp]).[5:supp][6:supp] Two observations follow. First, proposition chunking was *not* the top scorer — adaptive chunking scored materially higher, with non-overlapping CIs (Adaptive's 2.37 exceeds Proposition's upper bound of 2.33; Proposition's 2.07 falls below Adaptive's lower bound of 2.10) and a higher F1 (0.63 vs 0.49).[7:supp] Second, Proposition and Semantic chunking were statistically indistinguishable (overlapping CIs). This is a caution against over-claiming that any single granularity dominates; the effect of chunking is itself domain- and metric-dependent. Notably all three advanced strategies cleared the Basic RAG baseline, so the comparison does corroborate the directional claim that *some* deliberate chunking beats naive fixed-size chunking.

The atomic-fact family also includes evaluation-side uses of the same primitive.[8:supp] FActScore "breaks a generation into a series of atomic facts and computes the percentage of those atomic facts supported by a reliable knowledge source" ([D20:supp]); the Atomic fact decomposition-based Retrieval and Editing (ARE) framework "decomposes generated long-form answers into molecular clauses and atomic facts using instruction-tuned Large Language Models" ([D21:rel]); and the Atomic Fact Extraction and Verification (AFEV) framework "iteratively decomposes complex claims into atomic facts, enabling fine-grained retrieval and adaptive reasoning" ([D22:supp]), reporting "state-of-the-art performance in both accuracy and interpretability" ([D26:supp]). What unites this family is the conviction that the atomic fact is the right unit of verification, retrieval, and reasoning; where they differ is *when* decomposition happens — at evaluation/answer time (FActScore, AFEV, ARE) or at ingest (OKT, making the atomic fact the durable stored object).[9:rel][1:rel][10:rel]

### 2.2 Knowledge graph construction paradigms

The KG construction field is organized around two dominant strategies. **Top-down ontology mapping** (DBpedia, Wikidata, YAGO) maps text into a pre-existing structured ontology. YAGO is "an ontology that combines high coverage with high quality, using Wikipedia as its core lexicon" ([D27:rel], [D32:supp]); its logical model "allows for computing a unique smallest base for any given YAGO ontology" ([D29:supp]), and DBpedia uses YAGO as a taxonomic backbone to "connect extracted facts into a coherent whole" ([D28:supp]). WordNet "provides a clean and carefully assembled hierarchy of thousands of concepts" ([D30:supp]). The strengths are real (precise types, cross-domain consistency, queryable formal structure). The weaknesses are equally documented: "manually assembled sources … are costly to assemble and require continuous human effort to remain up to date, meaning no hand-crafted ontology can keep track of the most recent Windows version or the latest soccer star" ([D31:supp]); Freebase "depends on community acceptance and the discovery of effective ways to enforce uniformity across the ontology, as different contributors may prefer different ways of modeling reality" ([D34:supp]); DBpedia extracts "a wealth of facts, but the same relationship may appear under different names (e.g., length, length-in-km, length-km)" ([D35:supp]); and entity linking faces scale problems where "there are millions of possible entities to consider for each mention" ([D37:supp]).

**Bottom-up triplet extraction** (OpenIE, REBEL, Carta, EDC, AEVS, PKG) extracts (subject, relation, object) triples directly from text. OpenIE is "a Natural Language Processing (NLP) task aimed at deriving structured information from unstructured text, unrestricted by relation type or domain" ([D33:supp]); TextRunner "aims to extract all instances of all meaningful relations from Web pages" ([D36:supp]). The REBEL concept group ([concept:5cd9e0e7](<concept:5cd9e0e7-c8cb-4038-aa19-92813ba76227>)) frames relation extraction as autoregressive generation of "a sequence of linearized triplets given an input sentence" ([D38:supp]), overcoming error-propagation limits of pipeline and table-filling methods that "require the inference of all possible entity pairs" and "may predict multiple values for a single attribute … or incompatible relations" ([D39:supp], [D41:supp]). The EDC framework ([concept:608b5555](<concept:608b5555-4c65-442a-932d-9249d32e1d6a>)) implements "a three-phase extract-define-canonicalize pipeline performing open information extraction followed by post hoc schema canonicalization" ([D46:supp]), and tests "on three KGC benchmarks demonstrate that EDC extracts high-quality triplets without parameter tuning and handles significantly larger schemas" ([D40:supp]). The AEVS framework "ensures complete traceability by providing character-level provenance for every triplet element and enabling principled hallucination detection" ([D43:supp]). The Pseudo-Knowledge Graph (PKG) ([concept:0935787b](<concept:0935787b-219c-48ab-9411-aa7a5ea0119e>)) is built by a "PKG Builder [that] transforms raw data into a structured graph format by using a hybrid approach that combines traditional NLP algorithms ... with state-of-the-art language model techniques to identify entities and extract relationships from unstructured text" ([D45:supp]).

The weaknesses of triplet extraction are extensively documented.[11:supp][12:supp][13:rel] The distant-supervision methodology used in REBEL "introduces inherent noise and incompleteness, meaning not all triplets inferable from the text are necessarily present in the reference annotations" ([D42:supp]). Wiki-NRE is "a distantly supervised KGC benchmark with a constrained relation schema of 45 unique relation types" ([D44:supp]) — illustrating relation-type sparsity.[14:supp] Existing LLM-driven KG construction "predominantly employ stateless batch processing pipelines, which exhibit structural deficiencies in cross-chunk semantic relation capture, entity disambiguation, and construction process interpretability" ([D47:supp]). The construction of KGs "is challenging and often requires considerable human effort and domain expertise" ([D48:supp]), and "the KG schema must be included in the LLM prompt to generate valid triplets, and larger, more complex schemas can easily exceed the context window length" ([D49:supp]).[15:rel][16:supp] The bridge concept capturing the triplet paradigm's weakness is Generative Knowledge Extraction (GKE) ([concept:c972f702](<concept:c972f702-76b8-4a17-bcd6-d214338a3737>)), whose synthesis documents that GKE is prone to "entity recognition failures, relational semantic distortions, unsupported hallucinations, and representation inconsistencies" ([D50:supp]); hallucinations such as "(Steve Jobs, nationality, American) or (Steve Jobs, education, Reed College) that are not in the source text … are difficult to detect automatically, may introduce misinformation, and undermine trust in the knowledge graph" ([D51:supp]). A 2026 ACL study (the GraphRefine source, [concept:7a01f6de](<concept:7a01f6de-34b1-4453-adf0-a47d9e4f527f>)) responds to these errors with a model-agnostic refinement stage that applies four operations (KEEP/DELETE/FIX/REWRITE) to draft triples ([D52:supp]) — evidence that the field itself treats the triplet output as needing a second corrective pass.

(Agent hypothesis, not stated by any source: the persistent need for a refinement stage in the triplet literature — GraphRefine, AEVS's restoration verification ([D53:supp]), EDC's post-hoc canonicalization — suggests a structural pattern in which the *relation label* is the single most error-prone element of a triplet, because it is the one element not anchored to a surface span in the source text.[17:rel][18:rel] The facts above describe errors in each triplet element, but no source in the repository isolates the relation label as the dominant failure locus; confirming this would require a per-element error breakdown from GraphRefine or AEVS ablations, which the provided facts do not give.)

### 2.3 Bipartite network projections and co-occurrence analysis

The mathematical machinery underlying OKT's concept graph is bipartite-network projection, documented in the repository's statistical-physics and bibliometric literature. The repository establishes that "statistical co-occurrence measures are used to infer relationships based on the frequency of entity co-occurrences" ([D54:supp]) and that "the indirect relation between two nodes belonging to the same set of a bipartite network can be measured through their co-occurrences (or common neighbors), which is the number of nodes of the other set they are both connected to" ([D57:supp]). The co-occurrence formula is given explicitly: "the co-occurrences of nodes i and j of set L in a bipartite network are given by Cij = ∑ α∈Γ MiαMjα" ([D56:supp]) — a monopartite projection of the bipartite fact-concept graph.[19:rel][20:rel][21:rel][22:rel][23:rel] The [Bipartite Network Projection Analysis](<concept:b660db2c-e277-45e0-9965-59ddc2401568>) concept cluster provides the exact mathematical machinery (Γ-projection, BiCM null models, statistical validation of co-occurrences) that OKT's `concept_relations` materialized view instantiates.

The central caveat this literature imposes: "nodes with high degree naturally tend to have more co-occurrences than low-degree nodes" ([D55:supp]) and "co-occurrences in bipartite network projections can be influenced by single node variables, which can make it difficult to understand if they indicate an effective interdependence between nodes" ([D59:supp]). The mature literature treats raw co-occurrence counts as insufficient: "to identify the most informative co-occurrences in a network, a statistically-grounded approach involves performing link validation using a null network model" ([D58:supp]), comparing each observed Cij against a null distribution and keeping only links whose p-value clears a threshold ([D60:supp], [D66:supp]).[24:supp][25:supp][20:rel][19:rel][3:supp] And "a main problem in studying bipartite network projections is that they are often very dense … any two nodes are connected in the projected network as soon as they have a single co-occurrence" ([D64:supp]). This is the validation challenge that the proposed structural experiment (§6) is designed to address.

---

## 3. Fact-RAG: Atomic Fact Decomposition as a Retrieval Unit[26:rel]

### 3.1 The OKT fact extraction design

OKT adopts the atomic-fact pole of the granularity spectrum with architectural commitments distinguishing it from both passage-chunking and proposition indexing.

**Extraction.** Facts are extracted during source decomposition by a prompted LLM. Each fact is an atomic, self-contained assertion (1–4 sentences), each carrying a full subject+predicate+claim, with source attribution folded in at extraction time. The design rule is strict self-containment: a fact must be verifiable from source text alone, without requiring neighboring facts to resolve pronouns or context.[27:rel][28:rel] The repository establishes that "the process of extracting information from a source text requires replacing all pronouns and implicit subjects with the explicit entity, person, concept, or topic name to ensure the fact is self-contained" ([D67:supp]). This is the *primary* extraction artifact of the pipeline, not a secondary index derived from pre-existing passages.[29:rel]

**Deduplication.** Facts are deduplicated via embedding similarity (Qdrant nearest-neighbor, threshold-based), so the same factoid surfaced across multiple sources collapses to a single fact row carrying multiple source links.

**Source attribution.** A fact-source junction (N:M) records provenance; a fact confirmed by N sources carries `source_count=N`. This is the structural feature that propositions, as defined by Dense X, do not natively carry — propositions are per-document index entries, not cross-source consolidated records.

**Hybrid retrieval.** Facts have both a tsvector full-text index (websearch_to_tsquery with AND semantics) and a Qdrant embedding; retrieval fuses lexical and dense results via reciprocal rank fusion.[30:rel]

**Concept linkage.** Facts feed a downstream concept graph (§4) — facts are linked to concepts, concepts are summarized and synthesized, and concepts relate to one another by shared facts. This is where OKT departs most sharply from proposition-based RAG: the atomic unit is a first-class citizen that participates in a graph above it, not merely a flat retrieval index.[27:rel][28:rel]

### 3.2 Comparison to Dense X propositions

Both designs decompose text into atomic, self-contained natural-language units, and both argue (or are argued for) as improvements over passage-level retrieval.[31:rel] The differences are architectural:

| Dimension | Dense X Propositions | OKT Atomic Facts |
|---|---|---|
| **Provenance of the unit** | Derived from existing passages by a fine-tuned "propositionizer" model | Extracted once at ingest from raw source text by a prompted LLM during source decomposition |
| **Lifecycle** | Re-derived per corpus / per query pipeline; a derived index | The primary extraction artifact; the first-class citizen |
| **Cross-source consolidation** | Per-document; no native dedup across documents | Deduplicated across sources via embedding similarity; one fact row, N source links (`source_count`) |
| **Attribution** | Propositions index passages; attribution is implicit via the passage they came from | Explicit N:M fact-source junction; `source_count` is a queryable confirmation signal |
| **Downstream structure** | Flat index feeding retrieval | Feeds a concept graph (extraction → embedding → dedup → concept linkage → summarization/synthesis) |
| **Retrieval path** | Dense retrieval over proposition embeddings | Hybrid: lexical tsvector + Qdrant embedding with reciprocal rank fusion |[4:supp]

Both approaches share the fine-grained bet. The repository's evidence for that bet comes primarily from Dense X's own experiments, which report that fine-grained indexing "significantly outperforms passage-level units" ([D8:supp]). The controlled clinical chunking comparison does *not* independently corroborate the fine-grained-wins reading once all four strategies are considered: proposition chunking placed second (accuracy 2.07) behind adaptive chunking (2.37), and was statistically tied with semantic chunking (2.04) — though all three advanced strategies beat the Basic RAG baseline (1.64) ([D24:supp], [D19:supp], [D25:supp], [D23:supp]).[27:rel][28:rel][4:supp] The clinical comparison supports the narrower claim that *some* deliberate chunking beats naive fixed-size chunking; it does not support the claim that the *finest* granularity (propositions) is best in that domain. OKT's contribution relative to Dense X is making the atomic unit the *first-class* object — with deduplication, source_count, and concept linkage — rather than a derived retrieval index bolted onto a passage pipeline.

### 3.3 MultiHop-RAG benchmark results[32:supp]

The OKT system was benchmarked on the full MultiHop-RAG dataset (n=2556 questions, 609 news articles), a benchmark authored by Tang and Yang (2024) for "retrieval-augmented generation for multi-hop queries" whose concept summary in the repository describes it as a dataset of news articles spanning September 2013 to December 2023 across technology, science, health, sports, business, and entertainment, with inference, comparison, temporal, and null query types ([Multi-Hop Retrieval-Augmented Generation](<concept:3446737c-4ea8-41a2-9f0e-3b787d664785>)). Six retrieval strategies were compared; results are reported as experiment metrics, cited as `[Experiment: MultiHop-RAG, n=2556]`.

| Configuration | Accuracy | Hallucination | Tokens/q |
|---|---|---|---|
| facts@20 (inference prompt) | 0.757 | 11.1% | 8830 |
| facts@10 (inference prompt) | 0.697 | 9.8% | 5083 |
| direct@20 (inference prompt) | 0.683 | 9.4% | 3312 |
| direct@10 (inference prompt) | 0.560 | 7.7% | 1931 |
| concept-first (aggressive prompt) | 0.395 | 9.6% | 6845 |
| facts@10 (aggressive prompt) | 0.749 | 11.6% | — |

*Source: [Experiment: MultiHop-RAG, n=2556]*[33:supp][34:supp]

Per question type (facts@20): inference 0.876, comparison 0.643, temporal 0.642, null 0.987 [Experiment: MultiHop-RAG]. The hallucination hotspot is comparison queries (~20% across all configs); inference queries sit below 3% [Experiment: MultiHop-RAG].

**Finding A — Hallucination control is structural.** Hallucination stays in the 7.7–11.1% band across *all four* fact/direct configurations, including the naive `direct@10` (7.7%) and `direct@20` (9.4%) baselines that perform no query-extraction or reasoning preprocessing. This is the most robust finding of the benchmark because it holds across configurations that differ in retrieval strategy, prompt type, and token budget.[35:rel] The implication the system design invites: hallucination control is a property of the atomic-fact chunking substrate, not of the query-extraction step layered above it. This converges with the repository's broader framing that "augmenting large language models by retrieving information from external knowledge resources is one promising solution to combat hallucinations and factual inaccuracies" ([D62:supp]) and that retrieval-augmented LMs "can better adapt to changes in world state and incorporate long-tail knowledge" ([D63:supp]) — but it sharpens the claim: granularity, not retrieval per se, is doing the work.

**Finding B — The fact budget is a free lever.** Doubling the retrieval budget from 10 to 20 lifts accuracy by +0.060 (facts: 0.697→0.757) and +0.123 (direct: 0.560→0.683), while hallucination moves only modestly (facts 9.8→11.1%, direct 7.7→9.4%) [Experiment: MultiHop-RAG]. The marginal-accuracy-per-marginal-hallucination trade-off is favorable.[36:contr]

**Finding C — Concepts are not the right substrate for targeted QA.** Concept-first retrieval scores 0.395 — dominated on both accuracy and cost (6845 tokens/q) — with only 34 unique wins out of 2556 questions [Experiment: MultiHop-RAG]. This is consistent with the repository's own tooling guidance that concept-first retrieval "is a discovery/exploration substrate, not a targeted-QA path." The reading: concepts serve synthesis and exploration (§4's domain), not retrieval optimization. Routing a targeted factual question through a concept graph loses the precision that direct fact retrieval preserves.

**Finding D — direct@20 is the cost/quality sweet spot.** At 0.683 accuracy and 3312 tokens/q, `direct@20` sits within 0.014 of `facts@10` (0.697 at 5083 tokens) at roughly 60% of the token cost [Experiment: MultiHop-RAG]. If the operating constraint is cost per unit accuracy, naive direct retrieval over the atomic-fact index is competitive with — and cheaper than — query-extraction-augmented fact retrieval.

### 3.4 The hallucination-control finding

The hallucination-control finding deserves isolation because it is the most robust result of the benchmark. It holds across *all four* fact/direct configurations — two retrieval strategies (facts vs. direct) × two budgets (10 vs. 20) — including configurations that perform no query preprocessing. This is precisely the property one would expect if hallucination control were a *structural* property of the chunking substrate: changing the retrieval strategy and prompt budget moves accuracy, but hallucination stays in a narrow band. The convergence with the broader repository framing that retrieval augmentation combats hallucination ([D62:supp]) is meaningful but the OKT finding is sharper — it attributes the effect to granularity, not to retrieval alone.

### 3.5 Honest limitations

These are strong numbers, but they come from a single configuration of a single benchmark with a single LLM backend (`google/gemma-4-31b-it` via OpenRouter; the original MultiHop-RAG benchmark by Tang & Yang used `gpt-4o-mini`, but all OKT re-runs reported here use gemma) on a single corpus (news articles) [Experiment: MultiHop-RAG]. The scoring is lenient — it does not require exact-match attribution.[37:supp] External baselines (Dense X propositions, traditional passage-chunk RAG, and late-chunked passages) have since been run on the same corpus/LLM/scoring setup (§3.7); the four fact/direct configurations are internal variants of the same atomic-fact index. Three cautions follow.

1. **The hallucination floor is a benchmark floor.** 7.7–11.1% is low, but the MultiHop-RAG hallucination measurement is the system's own classification; an external hallucination auditor (e.g., FActScore's automated estimator, which the repository reports has "less than 2% error rate" ([D61:supp])) applied to the same generations might classify differently.[38:supp][39:rel]
2. **The accuracy ceiling is untested.** Without an external comparator, we do not know whether 0.757 is good *for this benchmark* or merely good *for these configurations*. The concept summary for Multi-Hop RAG notes that the PKG framework's Meta-path Retrieval achieved 90.0 on a Qwen2.5-7b MultiHop-RAG ablation ([Multi-Hop Retrieval-Augmented Generation](<concept:3446737c-4ea8-41a2-9f0e-3b787d664785>)) — a different scoring scale and model, but a reminder that other systems report higher numbers on related tasks.
3. **Comparison queries are the failure mode.** The ~20% hallucination rate on comparison queries across all configs [Experiment: MultiHop-RAG] is the benchmark's pressure point. Atomic-fact chunking helps inference and null queries (0.876 and 0.987) far more than comparison (0.643), which may be precisely the query type where multi-fact *synthesis* — not single-fact retrieval — is the bottleneck.

### 3.6 Comparison to Dense X: convergence and divergence[40:rel]

**Convergence.** Dense X reports that fine-grained indexing "significantly outperforms passage-level units" ([D8:supp]); OKT's MultiHop-RAG results show atomic-fact retrieval holding hallucination low and accuracy competitive across four configurations [Experiment: MultiHop-RAG]. The clinical chunking comparison, read in full, does *not* place proposition chunking at the top — adaptive chunking scored highest (accuracy 2.37, F1 0.63), with proposition chunking second (2.07, F1 0.49) and statistically tied with semantic chunking (2.04, F1 0.46), all above the Basic RAG baseline (1.64, F1 0.21) ([D19:supp], [D24:supp], [D25:supp], [D23:supp]).[41:rel][35:rel] The clinical comparison therefore corroborates the *directional* claim (deliberate chunking beats naive fixed-size chunking) but *contests* the strongest version of the fine-grained-wins claim (that the finest granularity wins); in that domain an adaptive/semantic-ish strategy matched or beat propositions. Counting the clinical comparison as a third independent line for "fine-grained retrieval outperforms" would overstate what it shows.

**Divergence — where each might fail:**[32:supp]

- *OKT's vulnerability.* OKT extracts facts once at ingest with strict self-containment rules; if the extraction LLM drops a factoid or fuses two, the loss is durable and invisible at retrieval time. Dense X's propositionizer is a fine-tuned model operating on existing passages — a narrower, more reproducible transformation. OKT's bet is that ingest-time extraction is good enough; the MultiHop-RAG numbers are consistent with that bet, but the single-LLM, single-corpus setup means the bet is not stress-tested. The late-chunking critique applies *a fortiori* to OKT: if context is lost by atomization before embedding ([D13:supp]), OKT's self-contained facts — which deliberately strip surrounding context — are the maximal case of that loss.

- *Dense X's vulnerability.* Propositions are a derived index; they carry no cross-source consolidation, no source_count, and no graph above them. When two passages assert the same proposition, Dense X indexes two propositions; OKT collapses them into one fact with `source_count=2`. For multi-hop QA that rewards evidence convergence, OKT's consolidation is a feature; for retrieval that rewards coverage, Dense X's duplication is a feature. Which vulnerability bites depends on the task.

- *The context-collapse debate is symmetric.* PropRAG argues triples suffer "context collapse" and propositions fix it ([D16:supp]); late chunking argues *any* pre-embedding chunking causes context loss and late pooling fixes it ([D15:rel]). These are not mutually exclusive — propositions can be late-pooled — but they make opposite default bets, and neither has independent replication in this repository. Treating either as settled would be asymmetric.

---[28:rel]

### 3.7 Atomic Facts vs. Late-Chunked Passages (matched conditions)

The paper's §7.2 previously listed as "the single most important missing experiment for the fact-RAG contribution" a head-to-head between atomized-facts and late-pooled-passages on the same corpus, same LLM, same scoring.[27:rel] That experiment has now been run.

**Setup.** All four configs run on MultiHop-RAG (n=2556, 609 articles), generator LLM `google/gemma-4-31b-it` via OpenRouter, identical `has_intersection(gold, pred)` + refusal/hallucination rubric. Condition A (atomic facts) uses OKT's hybrid retrieval (tsvector + Qdrant RRF) over the `multihoprag` fact index with `google/gemini-embedding-2` (3072-dim). Condition B (late-chunked passages) uses dense-only cosine over a new `multihoprag_late_chunks` Qdrant collection built with `jinaai/jina-embeddings-v3` (1024-dim, self-hosted via transformers, late pooling per arXiv:2409.04701). The late-chunk window is 24 whitespace tokens, matched to the measured average OKT fact length (23.2 tokens; ratio 1.03×, well within the 2× granularity-match threshold). 95% bootstrap CIs (2000 resamples on per-question correctness) are reported so the effect is distinguishable from noise — a direct repeat of the paper's own critique of the clinical-chunking comparison, where CI overlap mattered.

**Methodological asymmetry (flagged).** Late chunking structurally requires a long-context embedding model that exposes pre-pooling token vectors; `gemini-embedding-2` returns pooled sentence vectors and cannot do late pooling. So Condition B uses `jina-embeddings-v3` (the model the late-chunking paper itself uses) while Condition A keeps `gemini-embedding-2`. This isolates (chunking strategy + its native embedding), not chunking alone. The asymmetry is inherent to the method, not a bug.

| Configuration | Accuracy | 95% CI | Hallucination | Refusal | Tokens/q |
|---|---|---|---|---|---|
| direct@10 (atomic facts) | 0.560 | [0.542, 0.580] | 7.7% | 47.9% | 1836 |
| direct@20 (atomic facts) | 0.683 | [0.665, 0.701] | 9.4% | 33.9% | 3202 |
| late_chunk@10 | 0.313 | [0.295, 0.331] | 3.4% | 77.0% | 1518 |
| late_chunk@20 | 0.386 | [0.368, 0.406] | 3.9% | 69.2% | 2746 |

*Source: [Experiment: Atomic Facts vs Late-Chunked Passages, n=2556] — full table, per-type breakdown, and CI-overlap verdict in `scripts/experiments/multihop_rag/results/late_chunk_experiment_results.txt`.*

**Accuracy — H1a supported with non-overlapping CIs.** Atomic facts outperform late-chunked passages by +0.247 (k=10) and +0.297 (k=20), with non-overlapping 95% CIs at both budgets. The paper can upgrade its accuracy claim from "beats naive fixed-chunking" to "beats the leading context-preserving alternative" — a materially stronger claim.

**Hallucination — late chunking wins, and the result cuts against Finding A's strongest reading.** Late chunking holds hallucination to 3.4% / 3.9% — roughly *half* the atomic-fact band (7.7–11.1%). The paper's Finding A ("hallucination control is structural to chunking") is corroborated but *sharpened in an unexpected direction*: late-chunked passages control hallucination *better* than atomic facts. The mechanism is visible in the per-type breakdown: late chunking cuts the comparison-query hallucination hotspot from ~17.8% / 20.0% (atomic facts) to 6.9% / 7.9% — the same query type the paper flagged (§3.5 #3) as the atomic-fact failure mode. The late-chunking critique (pre-embedding atomization throws away disambiguating context) appears to bite hardest on comparison queries, which require cross-fact synthesis, and late pooling preserves exactly that context.

**Refusal — the cost of low hallucination.** Late chunking's low hallucination comes with a high refusal rate (77% / 69% vs atomic facts' 48% / 34%): the generator abstains far more often when fed late-chunked passages. This is a different operating point on the abstention trade-off, not a pure win on either axis. Atomic facts buy accuracy at the cost of higher hallucination; late chunking buys hallucination control at the cost of accuracy and abstention.[28:rel]

**Verdict.** H0 (indistinguishable) is rejected on accuracy. H1a (atomic-facts win) is supported on accuracy with non-overlapping CIs. H1b (late-chunking wins) is supported on hallucination. Neither position dominates: they occupy different points on the accuracy/hallucination/refusal trade-off frontier. The experiment closes the paper's §7.2 open question with a nuanced split verdict rather than a clean win for either side.

**Caveats.** Single corpus (MultiHop-RAG news), single LLM (gemma-4-31b-it), embedding-model asymmetry (gemini vs jina) inherent to the late-chunking method, dense-only vs hybrid retrieval (the retrieval-strategy difference is part of what is compared). Do not extrapolate beyond these conditions.

**Note on the prompt confound (flagged).** The direct@10 / direct@20 numbers in the §3.7 table are the §3.3 headline numbers — same "inference prompt" (the cautious template the paper's Findings A–D were derived from), same fact index, same retrieval path.[42:rel] The late_chunk runs, however, were generated with the newer "simplified aggressive prompt" (commit to an answer when evidence is present, abstain only when absent — the current harness default). This is a prompt asymmetry: Condition A (atomic facts) uses the inference prompt; Condition B (late chunking) uses the aggressive prompt. The aggressive prompt *boosts* accuracy, so the prompt confound works *against* atomic facts — if late_chunk were re-run with the inference prompt, its accuracy would likely drop, *widening* the gap. The H1a verdict (atomic facts win on accuracy with non-overlapping CIs) is therefore conservative: the true matched-prompt gap is at least as large as the +0.247 / +0.297 reported here. The hallucination finding is in the same direction regardless of prompt: even against the §3.3 inference-prompt direct numbers (7.7% / 9.4%), late chunking's 3.4% / 3.9% is roughly half. A fully matched-prompt re-run of late_chunk under the inference prompt is the cleanest follow-up, but the gap is wide enough that the verdict is unlikely to invert.

---

## 4. Autonomous Concept-Graph Building from Atomic Facts

### 4.1 The concept extraction pipeline

After the upstream pipeline extracts and deduplicates atomic facts, an `extract_concepts` worker runs over stable facts and prompts an LLM to extract ALL concepts mentioned by each fact — people, places, molecules, orgs, methods, events, metrics, policies, technologies, diseases, eras. Each concept is emitted with a canonical name, an L3 context (an ontology-class label such as "Politician" or "Molecule"), and seed aliases. This mirrors the atomic-fact philosophy of the upstream stage: the repository establishes that extraction "requires replacing all pronouns and implicit subjects with the explicit entity, person, concept, or topic name to ensure the fact is self-contained" ([D67:supp]) and that propositions are "atomic expressions within text that each encapsulate a distinct factoid" ([D7:supp]). OKT's concept step operates on exactly such atomic, self-contained units.

**Canonicalization is text-search-based, not embedding-based or KB-linked.** A new (concept, context) pair is matched against existing concepts by `lower(canonical_name) OR lower(alias_text)`, scoped by `(repository_id, lower(context))`. A new "Donald Trump" with context "Politician" matches an existing "Donald Trump"/"Politician" via name+alias lookup. Merged concepts inherit aliases. A refinement step proposes canonical-name normalization and alias pruning. This is closer to the "frequency information, alias tables, and TF-IDF-based methods for candidate generation" that prior entity-linking work used ([D65:supp]) than to the dense bi-encoder retrieval that became state-of-the-art for large-scale linking ([D71:supp]). The deliberate trade-off: name+alias matching is cheap and interpretable, but it cannot resolve a new mention to an existing concept whose surface form differs and shares no alias — a known hard case that dense retrieval handles and string matching does not.

This is explicitly NOT entity linking to an external KB. The repository's own facts establish that entity linking, in the standard formulation, "given an input text document D ... and a list of entity mentions MD, the output of an entity linking model is a list of mention-entity pairs {(mi, ei)} ...[43:rel] where each entity e is an entry in a knowledge base (KB) such as Wikipedia" ([D72:rel]). OKT does the opposite: concepts are created bottom-up from the corpus and canonicalized by name+alias matching against *the same repository's* existing concepts, never against an external KB. The L3 "context" label sits ambiguously between a type and a soft tag — it functions as a scoping key for canonicalization but makes no claim to be a formal ontology class.[44:rel]

### 4.2 The bipartite fact-concept graph and the co-occurrence matview

The `concept_relations` materialized view computes, for every pair of concepts (A, B) that co-occur in at least one fact, `COUNT(DISTINCT fact_id) AS shared_fact_count`. The relation weight is the number of shared facts. No relation type is declared; no extraction step finds "A works_at B." The view is keyed on `lower(canonical_name)`, ordered pairs, per repository, refreshed periodically. `getRelatedConcepts(concept)` returns concepts ranked by `shared_fact_count` — structural neighbors in the fact-co-occurrence graph. The `concept_groups` table aggregates the same canonical name across multiple contexts (e.g., "DNA" as "Molecule" and "DNA" as "Concept"), presenting "one concept, many contexts."

What this is NOT: not triplets (no subject-predicate-object; no relation extraction); not a top-down ontology (the L3 context is a tag, not a schema); not OpenIE/REBEL/Carta (those extract (entity, relation, entity); OKT extracts (fact, concept) links); not entity linking to a KB.

### 4.3 The argument from first principles

The mechanism is a direct application of bipartite-network projection. The repository establishes that "statistical co-occurrence measures are used to infer relationships based on the frequency of entity co-occurrences" ([D54:supp]) and that "the indirect relation between two nodes belonging to the same set of a bipartite network can be measured through their co-occurrences" ([D57:supp]). The co-occurrence formula is "Cij = ∑ α∈Γ MiαMjα" ([D56:supp]) — a monopartite projection. OKT's `concept_relations` view is precisely this L-projection: concepts are set L, facts are set Γ, and `shared_fact_count` is Cij.

The argument from first principles: atomic facts are self-contained claims. A fact about "the Reserve Bank of Australia raising interest rates" mentions concepts {Reserve Bank of Australia, interest rate}. Another fact about "inflation data affecting RBA decisions" mentions {inflation, Reserve Bank of Australia}. The co-occurrence (Reserve Bank of Australia, inflation) with `shared_fact_count=2` emerges without any extraction step saying "RBA relates-to inflation." The relation is *implicit in the facts*. This scales: 100k facts → thousands of concepts → millions of co-occurrence pairs, all from a single LLM extraction pass per fact (one concept-extraction call per fact, plus one SQL view refresh), with no per-pair relation classification.

The repository's statistical-network facts frame the central caveat.[27:rel] In bipartite projections, "nodes with high degree naturally tend to have more co-occurrences than low-degree nodes" ([D55:supp]) and "co-occurrences in bipartite network projections can be influenced by single node variables" ([D59:supp]). The mature literature therefore treats raw co-occurrence counts as insufficient: "to identify the most informative co-occurrences in a network, a statistically-grounded approach involves performing link validation using a null network model" ([D58:supp]), comparing each observed Cij against a null distribution and keeping only links whose p-value clears a threshold ([D60:supp], [D66:supp]). And "a main problem in studying bipartite network projections is that they are often very dense ...[45:supp] any two nodes are connected in the projected network as soon as they have a single co-occurrence" ([D64:supp]). OKT's `concept_relations` view as described computes raw counts without null-model validation.[46:supp][47:rel][48:rel] This is a defensible engineering choice for a retrieval-aid graph (where high-degree concepts surfacing often is a feature, not noise), but it means the relation weights are confounded by concept frequency: a ubiquitous concept (e.g., "United States") will accumulate high `shared_fact_count` with many partners purely because it appears in many facts.

### 4.4 Comparison to triplet KGs: different structures, different tasks

This is a trade-off, not a clear win.[23:rel][20:rel] Both sides are built from their supporting evidence.

**Scenario A — the triplet paradigm is the stronger substrate for structured reasoning.** Under this reading, explicit relations are the foundation of everything downstream that makes a KG valuable: structured querying ("who works_at X"), link prediction (knowledge graph completion "aims to perform link prediction between entities" ([D68:supp])), and embedding-based reasoning (TransE/TransH "regard a relation as a translation from a head entity to a tail entity" ([D69:supp], [D70:supp])). The triplet's typed predicate is what makes the graph *queryable as a graph* rather than as a bag of co-occurring tags. Modern LLM-based KGC has pushed hard on quality: EDC extracts "high-quality triplets without parameter tuning and handles significantly larger schemas" ([D40:supp]); AEVS gives "character-level provenance for every triplet element" ([D43:supp]); GraphRefine applies four operations (KEEP/DELETE/FIX/REWRITE) to reduce the documented error classes. The PKG approach "integrates structured knowledge representations with unstructured natural language text" to let LLMs "process and interpret complex data more effectively" ([D73:supp]) and uses meta-path retrieval so that "relational paths help LLMs understand complex interactions" ([D78:supp]). Under this reading, relation semantics are worth their extraction cost because they are the substance of the graph; co-occurrence is, by comparison, a typeless correlation signal.

**Scenario B — the co-occurrence paradigm removes a structurally error-prone stage.** Under this reading, the relation *label* is the single most fragile element of a triplet, and removing the relation-extraction stage entirely removes a whole class of failure.[49:rel] The documented GKE errors — "relational semantic distortions" ([D50:supp]), the "(Steve Jobs, education, Reed College)" hallucinations not in source text ([D51:supp]), the AEVS "Unsupported Relation (S✓ R× O✓)" error where "both entities are grounded, but the relation lacks textual support" ([D80:supp]) — all localize to the predicate. Co-occurrence relations, being *computed* from shared facts rather than *extracted* from text, cannot suffer a "wrong relation label" because there is no label. They cannot be an unsupported-hallucination relation because they are not asserted as relations at all — they are observed co-presence.[50:supp][51:supp][52:supp] The PropRAG authors argue that structured RAG methods using "knowledge graphs built from triples are limited in knowledge representation fidelity due to the inherent context loss of triples, a phenomenon called context collapse" ([D16:supp]). OKT sidesteps context collapse by keeping the atomic fact as the primary object and treating the concept graph as a derived retrieval index, not as the knowledge representation itself.

**Comparison.** The two paradigms are not the same kind of structure and should not be compared on the same task. A triplet KG is a *knowledge base* — it stores knowledge in its typed relations; remove the triple and the knowledge is gone. OKT's concept graph is an *organization structure* — it does not store knowledge, it structures knowledge that already exists in the atomic facts. The concepts and their co-occurrence edges are a navigational index over the fact knowledge base, not an alternative to the triplet KG. Comparing them head-to-head on a QA task is a category error: the triplet KG competes with fact-RAG (both are knowledge bases), while the concept graph competes with other indexing/navigation structures (ontologies, embedding clusters, taxonomies). The fair comparison for the concept graph is on navigation and discovery tasks — "what else is in the repository about the concepts in this fact" — where the co-occurrence graph's typeless neighbors are exactly right, and a triplet KG's typed edges are over-specified. The fair comparison for a triplet KG is against fact-RAG on retrieval quality, where both store knowledge and compete to be the best retrieval substrate. The concept graph's value is in wide-domain navigation, synthesis, and exploration (§6), not in narrow-domain QA.

### 4.5 Comparison to top-down ontology: two parallel scenarios

Again, a trade-off presented symmetrically.

**Scenario A — the top-down ontology provides irreplaceable cross-domain consistency.** Under this reading, the formal structure of YAGO/DBpedia/Wikidata — "a clean and carefully assembled hierarchy of thousands of concepts" ([D30:supp]), with type checking that "can be used in a reductive fashion to eliminate implausible facts and in an inductive fashion to add supplemental facts to make the ontology consistent" ([D74:supp]) — is the substrate that makes knowledge reusable across applications and corpora. A "DNA" in repository A and a "DNA" in repository B mean the same thing because both link to the same Wikidata item, and "Wikidata item IDs can be used as language-independent identifiers to facilitate data exchange and integration across application boundaries" ([D77:supp]). The cost is real — "manually assembled knowledge sources … are costly to assemble and require continuous human effort" ([D31:supp]) — but it buys global identity, formal reasoning, and the "high-quality, multi-source ontology [that] could boost existing application performance and enable new applications" ([D75:supp]). Automatic extraction "often produces results of remarkable accuracy, but the quality remains significantly below that of hand-crafted knowledge bases" ([D76:supp]).

**Scenario B — OKT adapts to any corpus without ontology maintenance or entity-linking errors.** Under this reading, the top-down paradigm's central costs — ontology maintenance, failure on novel entities, entity-linking error propagation — are structural, and OKT avoids all three. There is no ontology to maintain: the L3 context is a per-fact LLM label, not a maintained schema, so the schema-in-context-window problem EDC identifies ("larger, more complex schemas can easily exceed the context window length" ([D49:supp])) does not arise. There is no entity linking step to fail: concepts are created in-repository, so the scale problem ("millions of possible entities to consider for each mention" ([D37:supp])) and the candidate-ranking trade-off ("when the number of retrieved candidates k is larger, the recall accuracy increases, but the ranking stage accuracy is likely to decrease" ([D79:supp])) do not apply. The system "adapts to any corpus" because nothing in the concept-extraction step presupposes a domain; the L3 labels are emitted per-fact, not drawn from a fixed vocabulary.[53:supp] The repository's survey frame supports this split: LLM-driven KGC is reviewed "from two perspectives: schema-based paradigms, which emphasize structure, normalization, and consistency; and schema-free paradigms, which highlight flexibility, adaptability, and open discovery" ([D81:rel]). OKT sits squarely in the schema-free column.

**Comparison.** The two paradigms trade cross-corpus identity for per-corpus adaptability. A top-down ontology gives you a "DNA" that is the same DNA everywhere, at the cost of a maintenance burden that "no hand-crafted ontology can keep track of" for fast-moving domains ([D31:supp]). OKT gives you a "DNA" that is independent per repository — each repo's "DNA" is created from that repo's facts and canonicalized only within it — at the cost of no cross-corpus concept identity.[54:rel][55:rel] There is no formal type hierarchy; the L3 context is a soft tag, so type-checking elimination of "implausible facts" ([D74:supp]) is not available. Whether this matters depends on whether the consumer needs global identity and formal reasoning (favors top-down) or fast, schema-free corpus-specific coverage (favors OKT). The repository does not contain an experiment comparing the two on the same corpus. Open question.

### 4.6 Failure modes — symmetric skepticism

The mandate is to apply skepticism symmetrically: where might bottom-up concept extraction *fail*? The repository gives several failure modes that apply directly to OKT's design, and they must be stated as plainly as the strengths.

**Concept fragmentation (the under-merging failure).** Because canonicalization is name+alias text matching scoped by context, two mentions of the same real-world concept that use different surface forms and share no alias will be created as *separate* concepts. This is the inverse of the DBpedia relation-fragmentation problem ([D35:supp]) but at the entity layer. The repository establishes that "systems that automatically extract knowledge structures from text often extract facts in a non-canonical form, meaning different identifiers are used for the same entity" ([D82:rel]) — OKT's alias-inheritance and refinement step is a mitigation, but a mention like "the RBA" in one fact and "the Reserve Bank of Australia" in another will only merge if "RBA" is captured as an alias; if the LLM omits it, the two fragment. Dense bi-encoder linking, which OKT deliberately does not use, is exactly the technique built to handle surface-form divergence ([D84:supp]).

**Over-merging by alias (the converse failure).** Aggressive alias inheritance can collapse distinct concepts that happen to share a surface form across contexts. The context scoping (`repository_id, lower(context)`) is the guard, but it is only as good as the L3 label. If the LLM labels two genuinely different "Washington"s with the same context, they merge.

**Context mislabeling.** The L3 context is LLM-assigned per fact. The repository documents that "fine-grained entity typing information helps in the entity linking process" ([D85:supp]) and that an oracle giving gold fine-grained types yields 98.6% accuracy on TACKBP-2010 ([D83:supp]). OKT has no oracle — the context is predicted, not gold — so context errors propagate into the canonicalization key and corrupt both merging and the multi-context grouping.

**LLM hallucinating concepts not in the text.** The same hallucination risk that plagues GKE triplets ([D51:supp]) applies to concept extraction: an LLM can emit a concept the fact does not actually mention, seeding a spurious node and spurious co-occurrence edges. Because the relation edges are *derived* from co-occurrence, a hallucinated concept poisons not just one node but every edge it forms with the real concepts in that fact. AEVS's answer to this — character-level provenance for every triplet element ([D43:supp]) — has no direct analogue in the described OKT concept step, which is a logical gap worth flagging.

**No relation semantics, no formal reasoning.** "Information-extraction approaches are more suitable for high coverage than for applications requiring consistent ontologies, such as automated reasoning or high-accuracy query processing, because no explicit logic-based knowledge representation model is available" ([D86:supp]).[56:rel] This fact, drawn from the YAGO literature, applies almost verbatim to OKT's co-occurrence graph — but the framing matters: the concept graph is an *organization structure*, not a knowledge base. It was never intended to support formal query processing over typed relations; that is the job of a knowledge base (triplet KG or fact store).[57:rel][58:rel][59:rel][60:rel] The concept graph's job is navigation — surfacing related concepts for an agent to explore. The absence of relation semantics is a design choice about what structure to build, not a deficiency in a structure that was trying to be a KG.

The mirror-image skepticism applies equally to the competitors and must not be one-directional: triplet KGs suffer *relational semantic distortions* ([D50:supp]) that OKT structurally cannot; top-down ontologies suffer *maintenance collapse* for fast-moving domains ([D31:supp]) that OKT structurally avoids. Each paradigm has a characteristic failure class the others do not share. Treating one paradigm's failures as disqualifying while treating another's as manageable would be the asymmetry this paper is required to avoid.[61:supp][62:supp]

---

## 5. Novel Properties of the Fact-Based Database[63:rel][62:supp]

This section highlights five novel **design-level properties** that a fact-based database enables — properties that follow from the OKT schema and are true by construction. They do not require the experiment (§6) to validate (though the experiment would measure their *practical impact*). For each, the contrast with triplet KGs and passage-chunk RAG is stated, and the symmetric skepticism (the failure mode the design does not eliminate) is given.

### 5.1 Fact co-occurrence relations (the core novelty)

**Property.** Relations between concepts are computed from fact co-occurrence (`shared_fact_count`) — the bipartite projection. No relation extraction step is performed. Relations are typeless (no relation schema).

**Why it's impossible in triplet KGs.** Triplet KGs require explicit relation extraction for every edge. The [Relation Extraction](<concept:5cd9e0e7-c8cb-4038-aa19-92813ba76227>) field — from pipeline systems through REBEL's seq2seq generation of "linearized triplets" ([D38:supp]) — exists precisely because triplet KGs need typed relations. EDC's "schema definition and post-hoc canonicalization" ([D90:supp]) step exists to manage the relation schema. OKT eliminates this entire pipeline. The co-occurrence relation is, in the bipartite-projection formalism, Cij = Σ_α M_iα M_jα ([D56:supp]) — "free" computation from the existing `fact_concepts` links.[45:supp]

**Why it's impossible in passage-chunk RAG.** Passage RAG has no concept-level relation structure at all; relations are implicit in the text of retrieved chunks and must be inferred by the LLM at query time.

**What it enables.** A navigable concept graph usable for exploration and synthesis without any relation-extraction engineering cost.

**Symmetric skepticism (mandatory).** Co-occurrence ≠ causation or even semantic relation. The bipartite-projection literature is explicit: "co-occurrences in bipartite network projections can be influenced by single node variables" ([D59:supp]) and high-degree nodes "naturally tend to have more co-occurrences than low-degree nodes" ([D55:supp]). Two concepts co-occurring in one fact may share no meaningful relationship — they were merely extracted from the same sentence. The relation is *statistical*, not *semantic*. Triplet KGs, for all their extraction cost, carry typed relations ("founded," "located-in") that are semantically interpretable.[64:supp][65:supp][66:supp] OKT's typeless relations are cheaper but less expressive. This is a genuine trade-off, not a pure advantage.[67:supp][68:supp][69:supp]

### 5.2 source_count as confidence / provenance signal

**Property.** A fact confirmed by N independent sources carries source_count=N. This is a structural confidence signal embedded in the data model.

**Why it's impossible in triplet KGs.** A triplet is one extraction from one source; there is no native multi-source confirmation field. Knowledge Graph Completion aims to "perform link prediction between entities" ([D68:supp]) — it predicts *missing* links, not *confidence in existing* links. KG embedding models like TransE "regard a relation as a translation from a head entity to a tail entity" ([D69:supp]) — the embedding encodes relation plausibility, not source multiplicity.

**Why it's impossible in passage-chunk RAG.** A passage chunk is from one document; there is no structural mechanism to confirm a claim across multiple retrieved chunks except by the LLM noticing overlap at query time.

**What it enables.** Retrieval prioritization by confirmation level; structural confidence weighting in synthesis.

**Symmetric skepticism.** source_count can be gamed by content syndication — the same claim republished across N outlets would yield source_count=N with no genuine independent confirmation. The repository's fact that "Wikidata allows conflicting data to coexist and provides mechanisms to organize this plurality" ([D89:supp]) highlights that multi-source is not the same as multi-*independent*-source. The confidence signal is real but requires source-independence analysis to be fully trustworthy.[70:supp][71:rel]

### 5.3 Provenance traceability to sentence level

**Property.** The `fact_references` table links each fact to specific sentence indices in specific sources. Any claim traces to exact sentences.[72:rel]

**Why it's impossible in passage-chunk RAG.** Passage RAG retrieves chunk-level provenance — coarser than sentence-level. The clinical-RAG chunking literature evaluates "advanced chunking for Retrieval-Augmented Generation" at the chunk granularity ([D92:supp]), not sentence-level.[73:supp]

**Why it's impossible (or rare) in triplet KGs.** Triplet KGs typically carry document-level provenance. The AEVS framework is a notable exception — it "ensures complete traceability by providing character-level provenance for every triplet element" ([D43:supp]) and "each triplet element must be provenance-linked to a specific character span" ([D91:supp]).[74:rel][75:rel][72:rel] AEVS demonstrates sentence/character-level provenance is *possible* for triplets, but it is not the default — it requires AEVS's explicit "extract-then-restore" architecture. OKT's sentence-level provenance is built into the default fact extraction pipeline.

**What it enables.** Synthesis claims that can be verified by clicking through to exact source sentences — the citation-enforced grounding that this paper itself relies on.[27:rel][28:rel]

**Symmetric skepticism.** Sentence-level provenance is only as good as extraction accuracy: wrong sentences linked = provenance theater. A mis-extracted fact that points at a sentence that does not actually support it is worse than no provenance, because it lends false credibility.[72:rel]

### 5.4 Multi-context concept groups

**Property.** The same concept appearing under multiple L3 contexts (e.g., "DNA" as Molecule and Concept) is aggregated into a `concept_group`. This is a *feature* revealing multifaceted nature.[72:rel]

**Why it's impossible in triplet KGs.** Triplet KGs typically force a single type per entity. The TransR authors observe that "an entity may have multiple aspects and various relations may focus on different aspects of entities" ([D87:supp]) — a recognized limitation of single-space KG embeddings.[75:rel][76:rel][77:rel][78:supp] OKT's multi-context design directly addresses this.

**What it enables.** A concept's multiple facets (as Algorithm, as Concept, as Model) are visible and navigable, not collapsed.

**Symmetric skepticism.** Multi-context can also be *fragmentation masquerading as multifacetedness*. The repository shows "scale-free network" (Concept) and "scale-free networks" (Model) as separate contexts ([concept:53fc24d6](<concept:53fc24d6-9952-42af-bdee-b4dc8c6fa3ba>)) — these may be genuine facets or may be extraction inconsistency. Measurement 6 (§6) is designed precisely to distinguish feature from bug. Without that audit, claiming multi-context is a "feature" is a design assertion, not an empirical finding.

### 5.5 Compounding knowledge

**Property.** New sources add facts that deduplicate against existing facts (incrementing source_count) and extract concepts that match existing concepts (extending the graph). The graph grows rather than replaces.

**Why it differs from triplet KGs.** KGs "are difficult to construct and evolve by nature, which creates challenges for existing KG methods to represent unseen knowledge and generate new facts" ([D88:supp]). Adding triples to a KG does not naturally merge with existing triples — each is a new extraction. OKT's dedup-merge is structural compounding.

**Why it differs from passage-chunk RAG.** Passage RAG adds chunks; each is a new retrieval target. There is no mechanism for a new chunk to *confirm* an existing chunk — they coexist as independent retrievable units.

**What it enables.** A knowledge structure that becomes denser and more confident with scale, rather than merely larger.

**Symmetric skepticism.** Compounding depends on deduplication quality. The repository documents that "contemporary approaches to document-level deduplication are either unreliable at accurately identifying duplicate documents or extremely expensive in terms of runtime and memory" ([D93:supp]). If dedup fails, compounding produces duplicates that inflate source_count spuriously or orphans that fragment the graph. The compounding property is real *conditional on dedup working* — which is exactly what Failure 4 (§6) measures.

---

## 6. Structural Experiment — Results

The structural experiment was executed on the `multihoprag` repository's derived concept graph: **18,241 concept groups, 36,075 facts, 94,581 bipartite fact↔concept edges, 76,233 co-occurrence relations** (max concept degree 644 = Google LLC; 70.2% of edges at weight 1; max weight 156).[79:rel] The graph under test is the MultiHop-RAG news corpus. Three experiment families were run: BiCM null-model edge validation (Experiment 1), seven graph-property measurements across four W thresholds (Experiment 2), and five failure-mode audits (Experiment 3) [Experiment: Graph Analysis]. All experiments are read-only — no OKT code or schema changes. The full protocol is then replicated on the `default` repository (§6.5) to test whether the structural findings generalize beyond the news domain.

### 6.1 BiCM null-model edge validation

**Question (§7.2 open question):** is raw `shared_fact_count` usable as-is, or must it be validated/thresholded? The report's own bipartite-projection theory says raw projections are dense and degree-confounded and require null-model validation ([D55:supp], [D58:supp]).

**Method.** The Bipartite Configuration Model (BiCM) is the degree-preserving null model for bipartite networks. Under it, the expected co-occurrence of concepts i, j is `⟨C_ij⟩ = k_i · k_j / |Γ|`. We compute z-scores and Poisson upper-tail p-values for every co-occurrence edge, then BH-adjust for multiple testing.[80:rel][33:supp]

**Metric 1 — Spearman correlation between raw weight and z-score: 0.0215** (p ≈ 3e-9; effectively zero). Raw `shared_fact_count` is almost uncorrelated with BiCM-validated structural significance. Ranking edges by raw weight is not a proxy for ranking by structural significance.

**Metric 4 — promotion (W≥10) vs BiCM significance cross-tab:**

| | significant (FDR<0.05) | confounded (|z|<1) | total |
|---|---|---|---|
| promoted (W≥10) | 747 | 0 | 747 |
| draft (W<10) | 73,477 | 152 | 75,486 |
| **total** | 74,224 | 152 | 76,233 |

The W≥10 promotion threshold is a **perfect precision filter** — all 747 promoted edges are structurally significant (0% confounded). But 73,477 significant edges are drafts — the W threshold discards 97% of the structurally real relations.

**Metric 5 — W-threshold sweep (recommended default W=5):** W=5 is the minimum W with zero confounded edges — the lowest W that is still a safe precision filter. It keeps 3,012 relations (4× the v1 W=10 backbone's 747), including the useful W=5..9 mid-band (Boston Dynamics↔Marc Raibert, Blizzard↔Diablo IV, Gaza↔IDF) that W=10 discards for zero precision gain. **Recommendation: lower the default promotion W from 10 to 5.**

**Degree-confounded edges (the report's predicted "USA↔everything" failure):** only 152 edges (0.2%) are degree-confounded, all at w=3–4 (USA↔Google, USA↔Amazon, USA↔OpenAI, Amazon↔AI). None reach w≥5. The failure is real but tiny.

### 6.2 Seven graph-property measurements across W thresholds

The measurements were run at four W thresholds (1, 2, 5, 10) because Experiment 1 showed W dramatically reshapes the edge set (76k → 23k → 3k → 747).[81:rel] A structural claim like "the graph is navigable" is only meaningful at a specific W.

**M1 — Degree distribution.** The report predicted "heavy-tailed but not strictly power-law." **Confirmed at all W.** The `powerlaw` package labels the fit "strongest" (α between 2 and 3), but the power-law-vs-lognormal comparison is decisively negative (R<0) at every W — **lognormal fits better than power-law**, exactly as Broido & Clauset found for most real-world networks. Hub concepts (Google k=655 at W=1) dominate connectivity.

**M2 — Connected components.** The report predicted "one giant component plus isolated clusters." **Confirmed at W=1 and W=2**: the giant holds 94.4% / 83.8% of nodes. At W=5 the giant still holds 72.2%, but at W=10 it collapses to 43.4% — the promoted backbone fragments into 89 small components. **W=5 is the last threshold where the graph has a dominant giant component.** The small components at W=1 are genuine topic islands (Mediterranean lifestyle, Whistler village), not extraction failures.[82:supp][83:supp]

**M3 — Community structure (highest priority per §6.3).** Modularity Q is high at all W (0.79–0.88), meaning strong community structure. **NMI vs corpus domains rises monotonically with W**: 0.41 at W=1, 0.45 at W=2, 0.57 at W=5, 0.59 at W=10. **Higher W = communities align more closely with corpus domains.** This confirms the report's prediction: co-occurrence projection produces semantically coherent clusters without any explicit topic assignment. NMI never exceeds 0.59 — communities capture topical structure that cuts across source domains (a sports team co-occurs with other teams regardless of the source).

**M4 — Small-world properties.** σ >> 1 at all W (1,270 at W=1, 109 at W=5, 22 at W=10). The giant component's average path length stays ~5 hops from W=1 to W=10 — **navigability holds at all W thresholds**. But the Van den Berg–van Leeuwen caveat applies: high σ is partly a sparseness artifact.

**M5 — Edge-weight distribution.** At W=1, 70.2% of edges are weight=1 (incidental co-occurrence).[84:rel][85:rel][86:rel] The backbone (W≥5) is uniformly high-weight. Cross-checked against Experiment 1's BiCM survivors.

**M6 — Concept fragmentation.** 7.0% of concept groups (1,282/18,242) appear under multiple L3 contexts. This is W-independent (a property of extraction, not the relation graph).

**M7 — source_count as confidence signal.** **No correlation** between fact source_count and concept centrality at any W (Pearson r ≈ 0.01). The reason is corpus-specific: 99.7% of facts have source_count=1. The measurement needs a multi-source-confirmation corpus to be meaningful.

### 6.3 Five failure-mode audits

Five gold-standard-grounded audits were run with DeepSeek V4 Flash as the LLM judge (the model-independent audits use no LLM) [Experiment: Graph Analysis].

**Failure 1 — Under-merging (fragmentation): 6.87% (1,254 pairs).** Detected via intra-corpus alias analysis (shared aliases, canonical-as-alias, prefix matching with zero shared facts). Examples: "sam" ↔ "samuel bankman-fried" (same person, split into two groups). The report called this "likely one of the two biggest risks" — present but moderate.

**Failure 2 — Over-merging (false merges): 4.0% (8/200).** Examples: "national basketball league" merges Australian NBL and US NBL (1937-1949); "the penguin" merges the fictional character with the TV series. The report called this "the second biggest risk" — present but rare.[87:rel]

**Failure 3 — Missing concepts (recall): 42% raw, 65% adjusted.** spaCy NER extracted gold entities from 100 facts; the LLM judge classified each "missed" entity as a real miss or noise. **59.3% of "missed" entities are spaCy noise** ("seven touchdown scoring drives", "memory", "storage", "that", "which"). Adjusted recall (excluding noise) rises to 65%, within the report's predicted 60-80% range. This quantifies why spaCy was removed from the system: it retrieved more entities but also more noise.

**Failure 3b — Hallucinated concepts (cross-contamination): 0.5% (1/200).** For each concept with >20 linked facts, 20 facts were sampled and checked for the concept's name or any alias (≥ 2 chars). Only 1 suspect concept found. The alias-aware method corrects a previous 55.5% false-positive rate (caused by name-format mismatch: OKT formalizes "google llc" while facts say "Google"). **Residual cross-contamination is negligible post-fix.**

**Failure 4 — Dedup-severed facts: skipped.** The `facts` table has no `content_hash` column in the current schema.

**Failure 5 — Context mislabeling: 13.5% (27/200).** The LLM judge assigned the correct L3 context from the repo's official 88-label shortlist. Errors are mostly over-generic labels ("person" instead of "Athlete", "organisation" instead of "Media publication").[88:rel] Within the report's predicted 10-25% range.

### 6.4 How the results inform the graph's purpose

The results confirm the report's §6.3 framing: **the concept graph's purpose is navigation and synthesis, not targeted fact retrieval.** The graph has strong, coherent structure (modularity Q = 0.79–0.88, NMI vs domains rising to 0.59, small-world σ >> 1, navigability in ~5 hops). W=5 is both the statistical sweet spot (Experiment 1: zero confounded edges) and the structural sweet spot (Experiment 2: last threshold with a dominant giant component at 72.2%). The concept graph's value is in wide-domain navigation — graph-walking to discover connected evidence for synthesis — not in narrow-domain QA where facts are the right substrate.

### 6.5 Replication on the `default` repository

To test whether the structural findings generalize beyond the news benchmark, the full protocol was re-run on the `default` repository's concept graph: **267,754 concept groups, 321,530 facts, 1,347,871 bipartite fact↔concept edges, 2,033,656 co-occurrence relations** — ~13× the `multihoprag` scale on concepts and ~27× on edges. The corpus is a multi-domain research collection (2,478 sources across food science, agriculture, politics, chemistry), not a news benchmark, so it stresses a different extraction regime (Latin species names, chemical compounds, heavy acronym use) [Experiment: Graph Analysis].

**Experiment 1 (BiCM) — replicates and sharpens.** Spearman ρ(raw weight, z-score) = **-0.0094** (vs 0.0215 on `multihoprag`; both effectively zero). 98.7% of edges survive FDR<0.05 (vs 97.4%).[89:supp] The W-threshold sweep shows the larger graph needs a **stricter** W to zero out confounded edges: W=16 on `default` vs W=5 on `multihoprag`. The core finding — raw `shared_fact_count` is degree-confounded and a W threshold is a clean precision filter — replicates; the threshold value is corpus-specific and grows with graph size.[90:rel][91:rel][89:supp][49:rel]

**Experiment 2 (graph properties) — structural findings replicate; W=1 skipped.** At 2M edges the W=1 graph exceeded the single-threaded networkx Louvain budget, so measurements run at W∈{2,5,10}. Findings: (M1) lognormal fits better than power-law at all W (Broido–Clauset, as on `multihoprag`); (M2) a dominant giant component holds through W=5 (89.9%) and W=10 (86.4%); (M3) modularity Q = 0.675–0.704 (vs 0.79–0.88 on `multihoprag` — slightly lower but still strong), NMI vs corpus domains rises monotonically with W (0.27 → 0.34 → 0.39, on 562 domains vs 609); (M4) small-world σ >> 1 at all W (σ=8071 at W=2, 440 at W=10), navigability in ~4–5 hops; (M7) source_count correlation ≈ 0 (99% of facts have source_count=1, same corpus limitation as `multihoprag`). The structural claims — heavy-tailed-but-lognormal, strong communities, small-world navigability, clean backbone at a corpus-specific W — all hold at 13× scale.[92:rel][93:rel]

**Experiment 3 (failure audits) — failure rates are corpus-sensitive.** Sample sizes: 300 for the LLM-judged audits (over-merge, recall, context mislabeling), 200 for the alias-aware hallucination check, full-graph for the SQL-only and alias audits. DeepSeek V4 Flash judge (same as `multihoprag`). Results vs `multihoprag`:

| Failure | `multihoprag` | `default` | Reading |
|---|---|---|---|
| 1 Under-merging (fragmentation) | 6.87% (1,254 pairs) | **13.9%** (37,212 pairs) | Higher — the multi-domain corpus has more surface-form variation (Latin names, acronyms), so the same canonicalization heuristic fragments more. |
| 2 Over-merging | 4.0% (8/200) | **0.3%** (1/300) | Lower — the technical corpus has fewer homonyms than the news benchmark (few "Apple"-the-company vs "Apple"-the-fruit collisions). |
| 3 Recall (adjusted) | 65% (100 facts) | **55.7%** (300 facts) | Lower — real-miss examples are dominated by technical tokens ("1-aminocycyclopropane-1-carboxylate deaminase", "gylfadóttir t") the conservative LLM extractor drops; noise rate 49.3% (spaCy over-extraction). |
| 3b Hallucination | 0.5% (1/200) | **0.0%** (0/200) | Negligible on both — the alias-aware method holds. |
| 5 Context mislabeling | 13.5% (27/200) | **16.0%** (48/300) | Within the same 10–25% band; errors skew over-generic ("person" instead of "Politician"). |

**What replicates and what does not.** The *structural* findings (BiCM degree-confounding of raw weights, lognormal degree, strong modularity, small-world navigability, clean backbone at a corpus-specific W) replicate cleanly at 13× scale — these are properties of the bipartite-projection mechanism, not of the corpus. The *extraction-quality* failure rates (fragmentation, recall, over-merging) are corpus-sensitive: a multi-domain technical corpus fragments more (13.9% vs 6.87%) and recalls less (55.7% vs 65%) than a news benchmark, while over-merging is rarer (0.3% vs 4.0%).[94:supp][95:supp] This is consistent with the paper's framing that the concept graph's structural quality is a property of the projection mechanism, while its extraction quality is a property of the LLM extractor interacting with corpus difficulty. Generalization across *further* domains remains an open question, but the structural claims now have two-corpus support rather than one.

---

## 7. Discussion

### 7.1 Where the evidence lands[96:rel][97:rel]

**Well-supported by convergent evidence.** The claim that *deliberate chunking outperforms naive fixed-size chunking* is supported by three independent lines: Dense X's own experiments ([D8:supp]), the controlled clinical chunking comparison (where all three advanced strategies — adaptive, proposition, semantic — beat the Basic RAG baseline) ([D19:supp], [D24:supp], [D25:supp], [D23:supp]), and OKT's MultiHop-RAG results. The convergence of two independent designs (OKT and Dense X) reaching the same granularity direction is stronger evidence than either alone. The claim that *hallucination control is structural to chunking* is the most robust OKT finding, because it holds across all four fact/direct configurations including the naive direct baseline [Experiment: MultiHop-RAG] — it survives single-config extrapolation precisely because it is *not* single-config. The claim that *the atomic fact is a viable first-class citizen* is supported by OKT's demonstration that dedup, source_count, and concept linkage can be built atop an atomic-fact substrate without sacrificing retrieval quality.[6:supp]

**Reliant on single-config extrapolation.** The 0.749/0.757 accuracy numbers come from one benchmark, one LLM (`google/gemma-4-31b-it`; the original MultiHop-RAG benchmark by Tang & Yang used `gpt-4o-mini`, but all OKT re-runs reported here use gemma), one corpus (news), with lenient scoring [Experiment: MultiHop-RAG]. They are suggestive, not decisive. External baselines (Dense X propositions, traditional passage-chunk RAG, and now late-chunked passages — §3.7) have been run on the same corpus/LLM/scoring setup; the late-chunking comparison is the discriminating experiment that was previously missing. The concept-first 0.395 result demonstrates concepts underperform facts *on this benchmark with this prompt*; it does not prove concepts are useless for QA in general, only that they are the wrong substrate for targeted factual retrieval when a fact index exists. The *stronger* claim that the *finest* granularity *dominates* other deliberate chunking strategies rests on fewer independent lines and is actively contested by the clinical comparison (where adaptive chunking outscored propositions).

**Where the two designs (OKT facts and Dense X propositions) might both be wrong — now tested.** The late-chunking rebuttal ([D15:rel], [D13:supp]) applies to *both*: both atomize before embedding and both risk losing the surrounding context that disambiguates a fragment. The experiment that was previously missing — atomized-facts vs. late-pooled-passages on the same corpus, same LLM, same scoring — has now been run (§3.7). The result is nuanced: atomic facts win on accuracy with non-overlapping CIs (H1a supported on accuracy), but late chunking wins on hallucination (3.4%/3.9% vs 7.7–11.1%), cutting the comparison-query hallucination hotspot from ~18–20% to ~7%. Neither position dominates; they occupy different points on the accuracy/hallucination/refusal trade-off frontier.

**The concept-graph layer: empirically validated on two corpora.** The structural experiment (§6) was executed on the `multihoprag` news benchmark and replicated on the `default` multi-domain research corpus (§6.5). Both confirm the design-level claims translate to usable structure: strong community coherence (modularity Q = 0.79–0.88 on `multihoprag`, 0.675–0.704 on `default`), navigability (small-world σ >> 1, ~5-hop paths), and a clean backbone at a corpus-specific W (W=5 on `multihoprag`, W=16 on `default`). The BiCM null-model validation answered the report's open question on both corpora: raw `shared_fact_count` is almost uncorrelated with structural significance (Spearman ρ = 0.02 on `multihoprag`, -0.009 on `default`), but a W threshold is a perfect precision filter — and the safe W grows with graph size. The failure-mode audits found the *structural* quality replicates while the *extraction-quality* failure rates are corpus-sensitive: fragmentation is higher on the multi-domain corpus (13.9% vs 6.87%), recall lower (55.7% vs 65%), over-merging lower (0.3% vs 4.0%), hallucination negligible on both, context mislabeling in the same 13–16% band. The concept graph's value is in navigation and synthesis, not targeted QA — confirmed by the structural measurements on both corpora and consistent with the MultiHop-RAG concept-first 0.395 score.

### 7.2 Open questions

**The late-chunking rebuttal — resolved (§3.7).** The previously-missing experiment (atomized-facts vs. late-pooled-passages on the same corpus, same LLM, same scoring) has now been run. The result is a split verdict: atomic facts win on accuracy with non-overlapping 95% bootstrap CIs (direct@10 0.560 [0.542, 0.580] vs late_chunk@10 0.313 [0.295, 0.331]; direct@20 0.683 [0.665, 0.701] vs late_chunk@20 0.386 [0.368, 0.406]), but late chunking wins on hallucination (3.4%/3.9% vs the atomic-fact band of 7.7–11.1%) at the cost of a much higher refusal rate (77%/69% vs 48%/34%).[98:rel] The granularity debate between atomic-self-contained and context-pooled-passage is no longer an open question — it is a documented trade-off: atomic facts buy accuracy at the cost of higher hallucination, late chunking buys hallucination control at the cost of accuracy and abstention. The embedding-model asymmetry inherent to late chunking (gemini-embedding-2 for facts vs jina-embeddings-v3 for late-chunked passages) is flagged in §3.7.[99:rel][100:rel][25:supp]

**The KG-vs-fact-RAG and concept-graph-vs-navigation comparisons.** The concept graph and a triplet KG are not the same kind of structure — the triplet KG is a knowledge base (stores knowledge in relations), the concept graph is an organization structure (indexes facts). A head-to-head between them on the same QA task is a category error.[89:supp][101:supp] The two open questions are: (1) does a properly-navigated triplet KG (with embeddings, entity linking, typed-path traversal) beat fact-RAG on retrieval quality? — both are knowledge bases and the fair comparison; (2) does the concept graph beat other indexing/navigation structures (ontologies, embedding clusters) on a discovery/navigation task? — the concept graph's value is in wide-domain navigation and synthesis, not narrow-domain QA.

**Null-model validation of co-occurrence links — answered, replicated.** The BiCM null-model validation was executed (§6.1) and replicated on the `default` corpus (§6.5).[28:rel] Raw `shared_fact_count` is almost uncorrelated with structural significance on both corpora (Spearman ρ = 0.02 on `multihoprag`, -0.009 on `default`), confirming the report's concern that raw weights are degree-confounded. On `multihoprag`, the W≥5 threshold is a perfect precision filter (zero confounded edges among 3,012 promoted edges), and 97.4% of all edges survive FDR<0.05. On `default` (2M edges), the safe W is higher (W=16 for zero confounded edges) and 98.7% survive FDR<0.05 — the finding replicates, but the threshold value is corpus-specific and grows with graph size. The recommendation is to lower the default promotion W from 10 to a corpus-calibrated value (5 on the news benchmark, higher on larger graphs) and optionally add a BiCM z-score column to the `concept_relations` matview so consumers can rank by significance, not just frequency.

### 7.3 The boundary between design claims and empirical claims

This paper draws a sharp boundary. The five novel properties (§5) are design-level and true by construction: co-occurrence relations are computed from the bipartite projection (a mathematical fact of the schema); source_count exists as a field (structurally present); sentence-level provenance is in the `fact_references` table (schema-guaranteed); multi-context concept groups are an explicit data structure; compounding is a consequence of dedup-merge semantics.[90:rel][89:supp] These are contributions *relative to triplet KGs and passage RAG*, grounded in the repository's evidence on those systems' limitations ([D82:rel], [D88:supp], [D69:supp]). The structural experiment (§6) was executed on the `multihoprag` news benchmark and replicated on the `default` multi-domain research corpus (§6.5), confirming the design-level claims translate to usable structure on both: the graph is heavy-tailed but not scale-free (lognormal fits better, as Broido & Clauset predict), has strong community coherence (NMI 0.41–0.59 vs corpus domains on `multihoprag`; 0.27–0.39 on `default`), is navigable (~5-hop paths at all W), and has a clean backbone at a corpus-specific W (W=5 on `multihoprag`, W=16 on `default`). The BiCM validation showed raw weights are almost uncorrelated with significance (ρ = 0.02 on `multihoprag`, -0.009 on `default`) but a W threshold is a safe precision filter. The failure audits found the structural quality replicates while extraction-quality failure rates are corpus-sensitive: fragmentation 6.87% (`multihoprag`) / 13.9% (`default`), over-merging 4.0% / 0.3%, hallucination 0.5% / 0.0%, context mislabeling 13.5% / 16.0%. The null-model validation question from §7.2 is answered on two corpora: raw `shared_fact_count` needs thresholding, and the data-driven default W is corpus-calibrated (5 on the news benchmark, higher on larger graphs).

The symmetric skepticism reveals that each novel property has a failure mode the design does not eliminate: co-occurrence relations are statistical, not semantic; source_count is gamed by syndication; sentence-level provenance is only as good as extraction accuracy; multi-context can be fragmentation; compounding depends on dedup. The honest framing: OKT's novel properties are *structural affordances that the schema makes possible* — they are not *guaranteed quality properties*. The experiment confirms the affordances translate to practical structure, while quantifying the failure rates.

---

## 8. Conclusion

This paper presented two interlocking contributions from the OKT system.[102:rel]

**The fact-RAG contribution** establishes that decomposing documents into atomic, self-contained, source-attributed facts is an effective retrieval granularity for multi-hop question answering, supported by MultiHop-RAG benchmark evidence (accuracy 0.757, hallucination 7.7–11.1% across all configurations, n=2556) [Experiment: MultiHop-RAG]. The most robust finding is that hallucination control is a *structural* property of the chunking substrate — it holds across configurations that differ in retrieval strategy, prompt type, and token budget, including a naive direct-retrieval baseline that performs no query preprocessing.[102:rel][103:rel] This converges with Dense X Retrieval's independent articulation of the fine-grained-retrieval bet, while the controlled clinical chunking comparison tempers the strongest version of that claim: in that domain, adaptive chunking outscored proposition chunking, and semantic chunking tied it, though all three advanced strategies beat the naive baseline. OKT's architectural contribution relative to Dense X is making the atomic unit the *first-class* object — with deduplication, source_count, and concept linkage — rather than a derived retrieval index bolted onto a passage pipeline. The late-chunking rebuttal, previously the single most important missing experiment, has now been run (§3.7): atomic facts win on accuracy (non-overlapping CIs, +0.25/+0.30 over late-chunked passages at k=10/20), but late chunking wins on hallucination (3.4%/3.9% vs 7.7–11.1%) at the cost of much higher refusal (77%/69% vs 48%/34%) — a split verdict that upgrades the paper's accuracy claim ("beats the leading context-preserving alternative") while qualifying the hallucination-control claim (late chunking controls hallucination *better* than atomic facts, so hallucination control is structural to chunking broadly, not unique to atomic-fact chunking).

**The autonomous concept-graph contribution** establishes a paradigm in which an LLM extracts *concepts* (as tags on atomic facts) and concept-to-concept relations are *computed* as co-occurrence counts rather than *extracted* as typed predicates. The result is a bipartite fact↔concept graph whose monopartite projection is a weighted co-occurrence network — a structure that emerges from which facts mention which concepts, with no relation-type declaration, no relation-classification step, and no external knowledge base to link against. This is a *trade-off*: it removes an entire failure-prone pipeline stage (relation extraction) and adapts to any corpus without a predefined schema, but it sacrifices relation semantics, inherits LLM extraction errors at the concept layer, and produces no cross-corpus concept identity. Crucially, the concept graph is an *organization structure* — a navigational index over the fact knowledge base — not a knowledge base itself. It should not be compared to a triplet KG on QA tasks: the triplet KG is a knowledge base (stores knowledge in relations), the concept graph is an index (structures knowledge already in facts).[41:rel][80:rel] The fair comparison for the concept graph is against other indexing/navigation structures on discovery and navigation tasks; the fair comparison for a triplet KG is against fact-RAG on retrieval quality. We presented the comparison to triplet KGs (REBEL, OpenIE, EDC, AEVS, GKE) and top-down ontology (DBpedia, Wikidata, YAGO) as parallel scenarios — each paradigm has a characteristic strength and a characteristic failure class the others do not share.[104:rel][105:rel] The five novel properties of the fact-based database (co-occurrence relations, source_count confidence, sentence-level provenance, multi-context concept groups, compounding) are design-level affordances that the schema makes possible; they are not guaranteed quality properties, and each carries a stated failure mode.

**The structural experiment** was executed on the `multihoprag` repository's concept graph (18,241 concepts, 76,233 co-occurrence relations) and replicated on the `default` repository (267,754 concepts, 2,033,656 co-occurrence relations). On both, BiCM null-model validation showed raw `shared_fact_count` is almost uncorrelated with structural significance (Spearman ρ = 0.02 / -0.009), but a W threshold is a perfect precision filter — and the safe W is corpus-calibrated (5 on `multihoprag`, 16 on `default`). Seven graph-property measurements confirmed on both corpora that the graph is heavy-tailed but not scale-free (lognormal fits better), has strong community coherence (NMI rising with W), is navigable (~5-hop paths at all W), and retains a dominant giant component through its safe W.[19:rel][20:rel][106:rel] Five failure-mode audits found the structural quality replicates while the extraction-quality failure rates are corpus-sensitive: fragmentation 6.87% (`multihoprag`) vs 13.9% (`default`), over-merging 4.0% vs 0.3%, hallucination 0.5% vs 0.0%, context mislabeling 13.5% vs 16.0%; adjusted recall 65% vs 55.7%. The experiment confirms the concept graph has usable structure for navigation and synthesis — its intended purpose — while quantifying the failure rates the report flagged as "biggest risks," and confirms the structural findings generalize beyond a single news benchmark.[62:supp][107:rel][108:rel]

**Net assessment.** The fact-RAG evidence is the most robust finding, now sharpened by the late-chunking experiment (§3.7): hallucination control is structural to *chunking broadly* (late-chunked passages achieve 3.4%/3.9% — even lower than atomic facts' 7.7–11.1%), while atomic facts' distinctive strength is *accuracy* (+0.25/+0.30 over late-chunked passages with non-overlapping CIs). The paper's accuracy claim is upgraded from "beats naive fixed-chunking" to "beats the leading context-preserving alternative"; the hallucination-control claim is corroborated but qualified — late chunking does it better, at the cost of accuracy and abstention. The concept-graph is a design contribution with structural experiments now executed: the graph has usable structure for navigation and synthesis (strong communities, ~5-hop navigability, clean backbone at W≥5), the BiCM validation answered the null-model question (raw weights need thresholding, W=5 is the data-driven default), and the failure audits quantified the two "biggest risks" as moderate (fragmentation 6.87%, over-merging 4.0%). The novel properties are design-level affordances that the experiments confirm translate to practical structure. The two contributions form a stack — atomic facts as the foundational unit, concepts extracted from facts, a co-occurrence graph emerging from shared facts — whose joint value is an architectural commitment the proposition literature and the triplet KG literature have not made. Generalization beyond the single news benchmark + single gemma LLM remains the open question the next round of investigation should answer.

---

## References

The references below are the key works cited throughout this paper, drawn from the OKT repository's ingested literature. Inline citations throughout the paper use the repository's fact and concept UUIDs (`<fact:UUID>`, `<concept:UUID>`) for precise provenance, and `[Experiment: MultiHop-RAG, n=2556]` for the OKT system's own benchmark results (which are system results, not repository facts).

- **Dense X Retrieval** (arXiv:2312.06648) — proposition-based retrieval unit ([D7:supp], [D8:supp], [D10:supp], [D9:supp], [D2:supp], [D1:supp]).
- **PropRAG** — context-rich propositions, multi-step reasoning chains, context-collapse argument ([D11:supp], [D12:supp], [D16:supp]).
- **Late chunking** (arXiv:2409.04701) — context-preserving chunking after long-context embedding ([D13:supp], [D14:supp], [D15:rel], [D17:supp]).
- **Clinical RAG chunking comparison** — four-strategy controlled comparison (Adaptive > Proposition > Semantic > Basic RAG) ([D18:supp], [D19:supp], [D24:supp], [D25:supp], [D23:supp], [D92:supp]).[72:rel][9:rel]
- **FActScore** — atomic-fact decomposition as evaluation primitive ([D20:supp], [D61:supp]).
- **ARE** — Atomic fact decomposition-based Retrieval and Editing ([D21:rel]).
- **AFEV** — Atomic Fact Extraction and Verification ([D22:supp], [D26:supp]).
- **MultiHop-RAG** (Tang & Yang, 2024) — multi-hop RAG benchmark ([Multi-Hop Retrieval-Augmented Generation](<concept:3446737c-4ea8-41a2-9f0e-3b787d664785>); [D97:rel], [D96:rel], [D94:supp]).
- **REBEL** — autoregressive triplet extraction ([Relation Extraction](<concept:5cd9e0e7-c8cb-4038-aa19-92813ba76227>); [D38:supp], [D39:supp], [D41:supp], [D42:supp], [D44:supp]).
- **OpenIE / TextRunner** — unrestricted relation extraction ([D33:supp], [D36:supp]).
- **EDC** — Extract-Define-Canonicalize LLM-based KGC ([concept:608b5555](<concept:608b5555-4c65-442a-932d-9249d32e1d6a>); [D46:supp], [D40:supp], [D90:supp], [D49:supp]).
- **AEVS** — character-level provenance triplet verification ([D43:supp], [D91:supp], [D80:supp], [D53:supp], [D95:supp]).
- **GKE / GraphRefine** — generative knowledge extraction errors and refinement ([Generative Knowledge Extraction](<concept:c972f702-76b8-4a17-bcd6-d214338a3737>); [GraphRefine](<concept:7a01f6de-34b1-4453-adf0-a47d9e4f527f>); [D50:supp], [D51:supp], [D52:supp]).
- **PKG** — Pseudo-Knowledge Graph with meta-path retrieval ([concept:0935787b](<concept:0935787b-219c-48ab-9411-aa7a5ea0119e>); [D45:supp], [D73:supp], [D78:supp]).
- **Triplet-RAG / SG-RAG** comparative multi-hop performance ([D98:supp], [D99:supp], [D100:supp]).[9:rel][1:rel]
- **KG embeddings** (TransE/TransH/TransR) — relation-as-translation ([D69:supp], [D70:supp], [D87:supp], [D68:supp], [D88:supp]).
- **KG construction limitations** ([D47:supp], [D48:supp], [D81:rel], [D101:supp]).
- **YAGO / DBpedia / Wikidata / WordNet / Freebase** — top-down ontology ([D27:rel], [D32:supp], [D29:supp], [D28:supp], [D30:supp], [D31:supp], [D34:supp], [D35:supp], [D37:supp], [D74:supp], [D77:supp], [D75:supp], [D76:supp], [D86:supp], [D89:supp]).
- **Entity linking** — mention-to-KB mapping, bi-encoder/cross-encoder retrieval ([D72:rel], [D65:supp], [D71:supp], [D84:supp], [D82:rel], [D79:supp], [D105:supp], [D106:supp], [D85:supp], [D83:supp]).
- **Bipartite network projection / co-occurrence analysis** — Γ-projection, BiCM null models, statistical validation ([Bipartite Network Projection Analysis](<concept:b660db2c-e277-45e0-9965-59ddc2401568>); [D54:supp], [D57:supp], [D56:supp], [D55:supp], [D59:supp], [D58:supp], [D60:supp], [D66:supp], [D64:supp], [D103:supp], [D102:supp], [D104:supp]).
- **Network science** — scale-free networks, clustering, community structure, small-world ([Scale-free network](<concept:06ef353f-73d6-4f25-9b87-805b3e34e066>), [Clustering coefficient](<concept:155a6fe7-9002-40c0-8ff4-08db4d41e201>), [Community structure](<concept:2572dd18-85e4-4b0d-bfaf-a7153f8fb5b6>), [Small-world network property](<concept:177f3557-7359-4eea-a8dd-a9303d09f1ba>); [D107:supp], [D110:supp], [D113:supp], [D109:supp], [D108:supp], [D111:supp], [D112:supp], [D114:supp], [D118:supp], [D115:supp], [D119:supp], [D116:supp], [D117:supp], [D121:supp], [D120:supp], [D122:supp]).
- **Deduplication** — document-level dedup reliability ([D93:supp]).
- **RAG framing** — hallucination mitigation, retrieval augmentation ([D4:supp], [D62:supp], [D63:supp], [D3:supp], [D5:supp], [D6:supp]).[109:supp][110:supp]
- **OKT fact extraction** — self-containment rule ([D67:supp]).

---

## Annex: Supporting Facts

### Direct cites

These facts were cited directly by the researcher or AI researcher in the text to support the claim. Each [D{M}] marker in the body links to a fact below.

**Dense X Retrieval: What Retrieval Granularity Should We Use?**
https://arxiv.org/abs/2312.06648

- [D1] (supports) The authors of 'Dense X Retrieval: What Retrieval Granularity Should We Use?' discovered that the choice of retrieval unit significantly impacts the performance of both retrieval and downstream tasks.
- [D2] (supports) In the context of using a learned dense retriever on a retrieval corpus at inference time, the retrieval unit (such as document, passage, or sentence) in which the corpus is indexed is a significant design choice.
- [D7] (supports) In the context of dense retrieval, propositions are defined as atomic expressions within text that each encapsulate a distinct factoid and are presented in a concise, self-contained natural language format.
- [D8] (supports) Experiments conducted by the authors of 'Dense X Retrieval: What Retrieval Granularity Should We Use?' reveal that indexing a corpus by fine-grained units, such as propositions, significantly outperforms passage-level units in retrieval tasks.
- [D10] (supports) According to the authors of 'Dense X Retrieval: What Retrieval Granularity Should We Use?', constructing prompts with fine-grained retrieved units for retrieval-augmented language models improves the performance of downstream question answering (QA) tasks given a specific computation budget.

**Dense Passage Retrieval for Open-Domain Question Answering**
https://arxiv.org/abs/2004.04906

- [D3] (supports) Open-domain question answering relies on efficient passage retrieval to select candidate contexts, for which traditional sparse vector space models, such as TF-IDF or BM25, are the de facto method.

**PropRAG: Guiding Retrieval with Beam Search over Proposition Paths**
https://doi.org/10.18653/v1/2025.emnlp-main.317

- [D4] (supports) Retrieval Augmented Generation (RAG) is the standard approach for equipping Large Language Models (LLMs) with up-to-date knowledge.
- [D11] (supports) PropRAG is a Retrieval Augmented Generation (RAG) framework that utilizes context-rich propositions instead of triples and employs an efficient, LLM-free online beam search over proposition paths to discover multi-step reasoning chains.
- [D12] (supports) PropRAG achieves state-of-the-art zero-shot Recall@5 and F1 scores on the 2Wiki, HotpotQA, and MuSiQue datasets by coupling a higher-fidelity knowledge representation with explicit path discovery.
- [D16] (supports) The authors of the PropRAG paper argue that structured RAG methods using knowledge graphs built from triples are limited in knowledge representation fidelity due to the inherent context loss of triples, a phenomenon called context collapse.
- [D94] (supports) Standard Retrieval Augmented Generation (RAG), which relies on independent passage retrieval, often fails to capture the interconnected nature of information required for complex, multi-hop reasoning.

**Improving language models by retrieving from trillions of tokens**
https://arxiv.org/abs/2112.04426

- [D5] (supports) The Retrieval-Enhanced Transformer (RETRO) enhances auto-regressive language models by conditioning on document chunks retrieved from a large corpus based on local similarity with preceding tokens.

**Late Chunking: Contextual Chunk Embeddings Using Long-Context Embedding Models**
https://arxiv.org/abs/2409.04701

- [D6] (supports) Practitioners often split text documents into smaller chunks and encode them separately to facilitate retrieval of smaller portions of text.
- [D9] (supports) Dense vector-based retrieval systems often perform better with shorter text segments because the semantics are less likely to be over-compressed in the embeddings.
- [D13] (supports) Chunk embeddings created by splitting documents and encoding them separately can lose contextual information from surrounding chunks, resulting in sub-optimal representations.
- [D14] (supports) Late chunking is a method that leverages long context embedding models to first embed all tokens of a long text, with chunking applied after the transformer model and just before mean pooling.
- [D15] (related) Chunk embeddings produced by late chunking capture full contextual information, leading to superior results across various retrieval tasks.
- [D17] (supports) Late chunking is a generic method that can be applied to a wide range of long-context embedding models and works without additional training.

**Comparative Evaluation of Advanced Chunking for Retrieval-Augmented Generation in Large Language Models for Clinical Decision Support**
https://doi.org/10.3390/bioengineering12111194

- [D18] (supports) In a comparative evaluation of advanced chunking for Retrieval-Augmented Generation (RAG) in Large Language Models for Clinical Decision Support, four otherwise identical RAG pipelines were built that differed only in how documents were segmented to allow direct measurement of chunking effects independent of model or retriever bias.
- [D19] (supports) In the study 'Comparative Evaluation of Advanced Chunking for Retrieval-Augmented Generation in Large Language Models for Clinical Decision Support' (published in Bioengineering 2025, 12, 1194), the Adaptive chunking strategy achieved a mean Accuracy of 2.37 [95% CI 2.10–2.60], a mean Relevance of 2.90 [95% CI 2.73–3.00], and IR metrics consisting of a Precision of 0.50 [0.31–0.68], Recall of 0.87 [0.69–1.00], and F1 score of 0.63 [0.36–0.78].
- [D23] (supports) In the study 'Comparative Evaluation of Advanced Chunking for Retrieval-Augmented Generation in Large Language Models for Clinical Decision Support' (published in Bioengineering 2025, 12, 1194), the Basic RAG chunking strategy achieved a mean Accuracy of 1.64 [95% CI 1.40–1.90], a mean Relevance of 2.60 [95% CI 2.33–2.87], and IR metrics consisting of a Precision of 0.17 [0.04–0.32], Recall of 0.40 [0.10–0.70], and F1 score of 0.21 [0.00–0.39].
- [D24] (supports) According to Table 1, the Proposition chunking strategy achieved a mean accuracy of 2.07 [95% CI 1.80–2.33], a mean relevance of 2.80 [2.57–2.97], a precision of 0.38 [0.21–0.57], a recall of 0.71 [0.46–0.93], and an F1 score of 0.49 [0.25–0.67].
- [D25] (supports) According to Table 1, the Semantic chunking strategy achieved a mean accuracy of 2.04 [95% CI 1.77–2.33], a mean relevance of 2.87 [2.70–3.00], a precision of 0.33 [0.16–0.52], a recall of 0.75 [0.50–1.00], and an F1 score of 0.46 [0.19–0.64].
- [D92] (supports) Quantitative benchmarking for the comparative evaluation of advanced chunking for Retrieval-Augmented Generation (RAG) in Large Language Models for Clinical Decision Support used thirty representative postoperative rhinoplasty queries to evaluate accuracy, relevance, and information-retrieval metrics under controlled conditions.

**FActScore: Fine-grained Atomic Evaluation of Factual Precision in Long Form Text Generation**
https://arxiv.org/abs/2305.14251

- [D20] (supports) FACTSCORE is an evaluation method for long-form text generation that breaks a generation into a series of atomic facts and computes the percentage of those atomic facts supported by a reliable knowledge source.
- [D61] (supports) The authors of the FACTSCORE paper introduced an automated model that estimates FACTSCORE using retrieval and a strong language model, which has less than a 2% error rate.

**Atomic Fact Decomposition Helps Attributed Question Answering**
https://arxiv.org/abs/2410.16708

- [D21] (related) The Atomic fact decomposition-based Retrieval and Editing (ARE) framework decomposes generated long-form answers into molecular clauses and atomic facts using instruction-tuned Large Language Models (LLMs).

**Fact in Fragments: Deconstructing Complex Claims via LLM-based Atomic Fact Extraction and Verification**
https://arxiv.org/abs/2506.07446

- [D22] (supports) Atomic Fact Extraction and Verification (AFEV) is a framework proposed to address the limitations of traditional fact verification by iteratively decomposing complex claims into atomic facts, enabling fine-grained retrieval and adaptive reasoning.
- [D26] (supports) Extensive experiments on five benchmark datasets demonstrate that the Atomic Fact Extraction and Verification (AFEV) framework achieves state-of-the-art performance in both accuracy and interpretability.

**YAGO: A Large Ontology from Wikipedia and WordNet**
https://doi.org/10.1016/j.websem.2008.06.001

- [D27] (related) YAGO is an ontology that combines high coverage with high quality, using Wikipedia as its core lexicon.
- [D28] (supports) DBpedia uses YAGO as a taxonomic backbone to connect extracted facts into a coherent whole.
- [D29] (supports) The logical model of the YAGO ontology allows for computing a unique smallest base for any given YAGO ontology.
- [D30] (supports) WordNet provides a clean and carefully assembled hierarchy of thousands of concepts.
- [D31] (supports) Manually assembled knowledge sources satisfy the highest quality expectations, but they are costly to assemble and require continuous human effort to remain up to date, meaning no hand-crafted ontology can keep track of the most recent Windows version or the latest soccer star.
- [D32] (supports) The construction of the YAGO ontology is a two-stage process: (1) different heuristics are applied to Wikipedia to extract candidate entities and candidate facts, which also establishes the connection between Wikipedia and WordNet, and (2) quality control techniques are applied.
- [D34] (supports) The effectiveness of the Freebase project depends on community acceptance and the discovery of effective ways to enforce uniformity across the ontology, as different contributors may prefer different ways of modeling reality.
- [D35] (supports) Because DBpedia uses words from infoboxes as relation names, it can extract a wealth of facts, but the same relationship may appear under different names (e.g., length, length-in-km, length-km).
- [D36] (supports) TextRunner aims to extract all instances of all meaningful relations from Web pages, a paradigm known as machine reading.
- [D74] (supports) Type checking in YAGO can be used in a reductive fashion to eliminate implausible facts and in an inductive fashion to add supplemental facts to make the ontology consistent.
- [D75] (supports) The availability of a high-quality, multi-source ontology could boost existing application performance and enable new applications in the Semantic Web era.
- [D76] (supports) Automatic knowledge acquisition for extracting knowledge structures often produces results of remarkable accuracy, but the quality remains significantly below that of hand-crafted knowledge bases.
- [D82] (related) Systems that automatically extract knowledge structures from text often extract facts in a non-canonical form, meaning different identifiers are used for the same entity and no clearly defined relations exist.
- [D86] (supports) Information-extraction approaches are more suitable for high coverage than for applications requiring consistent ontologies, such as automated reasoning or high-accuracy query processing, because no explicit logic-based knowledge representation model is available.

**A Survey on Open Information Extraction from Rule-based Model to Large Language Model**
https://arxiv.org/abs/2208.08690

- [D33] (supports) Open Information Extraction (OpenIE) is a Natural Language Processing (NLP) task aimed at deriving structured information from unstructured text, unrestricted by relation type or domain.

**Scalable Zero-shot Entity Linking with Dense Entity Retrieval**
https://doi.org/10.18653/v1/2020.emnlp-main.519

- [D37] (supports) Scale is a key challenge for entity linking because there are millions of possible entities to consider for each mention.
- [D65] (supports) Prior work in entity linking has used frequency information, alias tables, and TF-IDF-based methods for candidate generation.
- [D71] (supports) Bi-encoder entity linking using approximate nearest neighbor search can link over 5.9 million candidates in 2 milliseconds.
- [D72] (related) In entity linking, given an input text document D = {w1, ..., wr} and a list of entity mentions MD = {m1, ..., mn}, the output of an entity linking model is a list of mention-entity pairs {(mi, ei)}i∈[1,n], where each entity e is an entry in a knowledge base (KB) such as Wikipedia.
- [D79] (supports) In entity linking, when the number of retrieved candidates k is larger, the recall accuracy increases, but the ranking stage accuracy is likely to decrease.
- [D83] (supports) Raiman and Raiman (2018) report 98.6% accuracy on TACKBP-2010 if fine-grained entity type information is provided by an oracle at test time.
- [D84] (supports) The zero-shot linking algorithm introduced by Ledell Wu et al. consists of two stages where each entity is defined only by a short textual description: (1) a first stage that performs retrieval in a dense space using a bi-encoder to independently embed the mention context and the entity descriptions, and (2) a second stage where each candidate is re-ranked with a cross-encoder that concatenates the mention and entity text.
- [D85] (supports) Work by Raiman and Raiman (2018), Onoe and Durrett (2019), and Khalife and Vazirgiannis (2018) demonstrates that fine-grained entity typing information helps in the entity linking process.
- [D105] (supports) Table 8 from 'Scalable Zero-shot Entity Linking with Dense Entity Retrieval' illustrates entity prediction examples where the bi-encoder model often predicts a shorter, more general entity (e.g., 'Ronaldo', 'Gothic fiction', 'Ancient Greek') while the cross-encoder predicts a more specific entity (e.g., 'Cristiano Ronaldo', 'Gothic art', 'Ancient Greek philosophy') based on the provided mention.
- [D106] (supports) In a qualitative analysis of entity linking, a bi-encoder mistakenly linked "Ronaldo" to a Brazilian football player, whereas a cross-encoder used the context word "Juventus" to correctly disambiguate the entity.

**REBEL: Relation Extraction By End-to-end Language generation**
https://doi.org/10.18653/v1/2021.findings-emnlp.204

- [D38] (supports) In the REBEL (Relation Extraction By End-to-end Language generation) model, the task is to autoregressively generate a sequence of linearized triplets given an input sentence x, defined by the formula: PBART(y | x) = product from i=1 to len(y) of PBART(yi | y < i, x).
- [D39] (supports) Pipeline and table filling methods for Relation Extraction require all possible entity pairs to be inferred, which can be computationally expensive.
- [D41] (supports) Pipeline and table filling methods for Relation Extraction may predict incompatible relations or multiple values for a single attribute, such as two birth dates for the same head entity.

**Extract, Define, Canonicalize: An LLM-based Framework for Knowledge Graph Construction**
https://doi.org/10.48550/arxiv.2404.03868

- [D40] (supports) Tests on three KGC benchmarks demonstrate that the Extract-Define-Canonicalize (EDC) framework extracts high-quality triplets without parameter tuning and handles significantly larger schemas compared to prior works.
- [D49] (supports) The researchers proposing the Extract-Define-Canonicalize (EDC) framework identify a principal issue in prior knowledge graph creation (KGC) methods: the KG schema must be included in the LLM prompt to generate valid triplets, and larger, more complex schemas can easily exceed the context window length of large language models (LLMs).
- [D90] (supports) The Extract-Define-Canonicalize (EDC) framework is a three-phase method for knowledge graph construction consisting of open information extraction, followed by schema definition and post-hoc canonicalization.

**Grounded Knowledge Graph Extraction via LLMs: An Anchor-Constrained Framework with Provenance Tracking**
https://doi.org/10.3390/computers15030178

- [D42] (supports) The distant supervision methodology used in the REBEL benchmark introduces inherent noise and incompleteness, meaning not all triplets inferable from the text are necessarily present in the reference annotations.
- [D43] (supports) AEVS is a three-stage framework for LLM-based knowledge graph extraction that ensures complete traceability by providing character-level provenance for every triplet element and enabling principled hallucination detection.
- [D44] (supports) Wiki-NRE is a distantly supervised knowledge graph construction (KGC) benchmark with a constrained relation schema of 45 unique relation types.
- [D46] (supports) EDC is a state-of-the-art LLM-based knowledge graph construction (KGC) framework that implements a three-phase extract-define-canonicalize pipeline performing open information extraction followed by post hoc schema canonicalization.
- [D51] (supports) Hallucinations in knowledge graph extraction, such as the model providing facts like (Steve Jobs, nationality, American) or (Steve Jobs, education, Reed College) that are not in the source text, pose risks because they are difficult to detect automatically, may introduce misinformation, and undermine trust in the knowledge graph.
- [D53] (supports) The AEVS framework architecture consists of three stages: (1) Stage 1 (anchor discovery) identifies entity, relation, and attribute anchors with character-level positions to form a closed vocabulary; (2) Stage 2 (grounded extraction) extracts triplets constrained by those anchors; and (3) Stage 3 (restoration-based verification and coverage-aware supplement) verifies triplets through independent restoration matching and ensures comprehensive extraction via a coverage-aware supplement mechanism targeting unused anchors.
- [D80] (supports) An 'Unsupported Relation (S✓ R× O✓)' error occurs when both entities are grounded, but the relation lacks textual support; for example, if a model extracts (Los Trancos Creek, named after, Las Trancas) from text stating only an etymological origin and not a 'named after' relation.
- [D91] (supports) The traceability constraint for the AEVS framework requires that each triplet element must be provenance-linked to a specific character span e of the triplet.
- [D95] (supports) In the Anchor Discovery process, relation anchors are mapped to canonical schema relations to enable normalization across diverse surface expressions, such as mapping 'founded', 'co-founded', and 'established' to the relation 'founder'.

**Pseudo-Knowledge Graph: Meta-Path Guided Retrieval and In-Graph Text for RAG-Equipped LLM**
https://arxiv.org/html/2503.00309v1

- [D45] (supports) The PKG Builder is an automated tool for constructing Pseudo-Knowledge Graphs (PKGs) that transforms raw data into a structured graph format by using a hybrid approach that combines traditional NLP algorithms (such as tokenization and dependency parsing) with state-of-the-art language model techniques to identify entities and extract relationships from unstructured text.
- [D54] (supports) Statistical co-occurrence measures are used to infer relationships based on the frequency of entity co-occurrences.
- [D73] (supports) The Pseudo-Knowledge Graph (PKG) framework is a novel approach designed to enhance semantic understanding, relation extraction, and information retrieval efficiency by integrating structured knowledge representations with unstructured natural language text, enabling LLMs to process and interpret complex data more effectively.
- [D78] (supports) By outlining relational paths, meta-path retrieval helps Large Language Models (LLMs) understand complex interactions and dependencies, enabling more accurate inference and deduction.
- [D96] (related) The MultiHop-RAG dataset is structured to evaluate the ability of models to synthesize information from disparate sources and generate coherent, contextually appropriate responses.
- [D97] (related) MultiHop-RAG is a benchmark dataset designed for multi-hop reasoning tasks that requires models to connect information across multiple documents to answer complex queries.

**RAGA: Reading-And-Graph-building-Agent for Autonomous Knowledge Graph Construction and Retrieval-Augmented Generation**
https://arxiv.org/abs/2605.17072

- [D47] (supports) Existing LLM-driven knowledge graph (KG) construction methods predominantly employ stateless batch processing pipelines, which exhibit structural deficiencies in cross-chunk semantic relation capture, entity disambiguation, and construction process interpretability, thereby undermining KG quality, retrieval precision, and deployment trust in high-stakes domains.

**Iterative Zero-Shot LLM Prompting for Knowledge Graph Construction**
https://arxiv.org/abs/2307.01128

- [D48] (supports) The paper 'Iterative Zero-Shot LLM Prompting for Knowledge Graph Construction' asserts that the generation of knowledge graphs is challenging and often requires considerable human effort and domain expertise, which hampers scalability and flexibility across different application fields.

**2026.acl-long.1353**
https://doi.org/10.18653/v1/2026.acl-long.1353

- [D50] (supports) Errors in knowledge graphs (KGs) constructed via generative knowledge extraction (GKE) include entity recognition failures, relational semantic distortions, unsupported hallucinations, and representation inconsistencies that reduce the clarity and usability of extracted triples.
- [D52] (supports) The GraphRefine framework for model factual inconsistency types in Knowledge Graphs (KGs) employs a fine-tuned LLM as a KG Refiner that takes a source document and a candidate triple as input and outputs one of four refinement operations: KEEP, DELETE, FIX, or REWRITE. According to the Figure 2 overview, the KG Refiner processes examples such as a triple regarding 'Space Mirror Memorial, located in, Florida' and produces corresponding operations: KEEP for correct triples, DELETE for null/null entries, FIX for triples with incorrect locations (e.g., changing California to Florida), and REWRITE for triples requiring modification (e.g., 'Mirror, located in Florida'). The refined outputs then undergo Graph Refinement using a KG Refiner and an Aggregation step to transform a draft KG into a Refined KG.

**Meta-validation of bipartite network projections**
https://doi.org/10.1038/s42005-022-00856-9

- [D55] (supports) In a bipartite network, nodes with high degree naturally tend to have more co-occurrences than low-degree nodes; more generally, the degree sequence of a network projection is highly dependent on the degree sequence of the two sets from the original bipartite structure.
- [D56] (supports) The co-occurrences of nodes i and j of set L in a bipartite network are given by the formula Cij = ∑ α∈Γ MiαMjα.
- [D57] (supports) The indirect relation between two nodes belonging to the same set of a bipartite network can be measured through their co-occurrences (or common neighbors), which is the number of nodes of the other set they are both connected to.
- [D58] (supports) To identify the most informative co-occurrences in a network, a statistically-grounded approach involves performing link validation using a null network model.
- [D59] (supports) Co-occurrences in bipartite network projections can be influenced by single node variables, which can make it difficult to understand if they indicate an effective interdependence between nodes.
- [D60] (supports) In bipartite network projections, the statistical significance of an observed co-occurrence value Cij > 0 is quantified by a p-value calculated as p[Cij] = 1 - ∑(x=0 to Cij-1) π(x|i, j), where π(⋅|i, j) is the probability distribution of expected co-occurrences between i and j under the null model.
- [D64] (supports) A main problem in studying bipartite network projections is that they are often very dense, making them difficult to handle with network theory tools because any two nodes are connected in the projected network as soon as they have a single co-occurrence.
- [D66] (supports) The procedure for assessing the statistical significance of observed co-occurrences involves placing a link on the validated network only when the p-value is smaller than the significance threshold p* against its null model expectation; this procedure is general and applies to any bipartite network.
- [D102] (supports) The process of filtering a bipartite network projection using a null model significance threshold drastically reduces the number of links, resulting in a sparser validated network with a clearer meaning.
- [D103] (supports) Under the Microcanonical partial model (Hypergeometric) for the projection of a bipartite network on set L, the mean value of the co-occurrences is hCij = (ki * kj) / |Γ|.
- [D115] (supports) When modularity is high enough, a robust community structure shared among the null models emerges in the analyzed bipartite networks.
- [D118] (supports) Broadly speaking, a community structure in a network is defined by sets of nodes, typically non-overlapping, characterized by having many more internal links (connecting nodes belonging to the same community) than external links (connecting nodes of different communities).
- [D119] (supports) The authors of the study found that different configuration model (CM) formulations for bipartite network projections lead to very different filtered networks, even when using the same validation threshold p*, despite being based on very similar null hypotheses.

**Active Retrieval Augmented Generation**
https://arxiv.org/abs/2305.06983

- [D62] (supports) Augmenting large language models (LMs) by retrieving information from external knowledge resources is one promising solution to combat hallucinations and factual inaccuracies.

**RAPTOR: Recursive Abstractive Processing for Tree-Organized Retrieval**
https://doi.org/10.48550/arxiv.2401.18059

- [D63] (supports) Retrieval-augmented language models can better adapt to changes in world state and incorporate long-tail knowledge.

**Cultural studies and yield attributes of pink oyster mushroom (Pleurotus djamor) in West Bengal**
https://bioresources.cnr.ncsu.edu/resources/cultural-studies-and-yield-attributes-of-pink-oyster-mushroom-pleurotus-djamor-in-west-bengal/

- [D67] (supports) The process of extracting information from a source text requires replacing all pronouns and implicit subjects with the explicit entity, person, concept, or topic name to ensure the fact is self-contained.

**Learning Entity and Relation Embeddings for Knowledge Graph Completion**
https://doi.org/10.1609/aaai.v29i1.9491

- [D68] (supports) Knowledge graph completion aims to perform link prediction between entities.
- [D69] (supports) Knowledge graph embedding models such as TransE and TransH build entity and relation embeddings by regarding a relation as a translation from a head entity to a tail entity.
- [D70] (supports) The authors of the paper 'Learning Entity and Relation Embeddings for Knowledge Graph Completion' propose TransR, a model that builds entity and relation embeddings in separate entity and relation spaces.
- [D87] (supports) The authors of the paper 'Learning Entity and Relation Embeddings for Knowledge Graph Completion' claim that a common semantic space is insufficient for modeling because an entity may have multiple aspects and various relations may focus on different aspects of entities.

**Wikidata: A Free Collaborative Knowledgebase**
https://cacm.acm.org/research/wikidata/

- [D77] (supports) Wikidata item IDs can be used as language-independent identifiers to facilitate data exchange and integration across application boundaries.
- [D89] (supports) Wikidata allows conflicting data to coexist and provides mechanisms to organize this plurality.

**LLM-empowered knowledge graph construction: A survey**
https://arxiv.org/abs/2510.20345

- [D81] (related) The survey 'LLM-empowered knowledge graph construction: A survey' reviews emerging LLM-driven approaches from two perspectives: schema-based paradigms, which emphasize structure, normalization, and consistency; and schema-free paradigms, which highlight flexibility, adaptability, and open discovery.

**Unifying Large Language Models and Knowledge Graphs: A Roadmap**
https://arxiv.org/abs/2306.08302

- [D88] (supports) Knowledge Graphs (KGs) are difficult to construct and evolve by nature, which creates challenges for existing KG methods to represent unseen knowledge and generate new facts.

**LSHBloom: Memory-efficient, Extreme-scale Document Deduplication**
https://arxiv.org/abs/2411.04257

- [D93] (supports) Contemporary approaches to document-level deduplication are either unreliable at accurately identifying duplicate documents or extremely expensive in terms of runtime and memory.

**SG-RAG MOT: SubGraph Retrieval Augmented Generation with Merging and Ordering Triplets for Knowledge Graph Multi-Hop Question Answering**
https://www.mdpi.com/2504-4990/7/3/74

- [D98] (supports) For Triplet RAG Top 5, the answer-matching rate (AMR) percentages were: Llama-3.1 8B Instruct (1-Hop: 54.94, 2-Hop: 4.58, 3-Hop: 9.85), Llama-3.2 3B Instruct (1-Hop: 52.24, 2-Hop: 5.27, 3-Hop: 12.83), Qwen-2.5 7B Instruct (1-Hop: 56.73, 2-Hop: 5.91, 3-Hop: 11.68), and Qwen-2.5 3B Instruct (1-Hop: 53.60, 2-Hop: 3.71, 3-Hop: 12.13).
- [D99] (supports) For Triplet RAG Top 20, the answer-matching rate (AMR) percentages were: Llama-3.1 8B Instruct (1-Hop: 63.87, 2-Hop: 7.18, 3-Hop: 14.63), Llama-3.2 3B Instruct (1-Hop: 61.46, 2-Hop: 7.12, 3-Hop: 16.79), Qwen-2.5 7B Instruct (1-Hop: 64.68, 2-Hop: 6.75, 3-Hop: 14.14), and Qwen-2.5 3B Instruct (1-Hop: 58.55, 2-Hop: 5.58, 3-Hop: 14.62).
- [D100] (supports) In an experiment using GPT-4 Turbo to measure the answer-matching rate (AMR), SG-RAG achieved scores of 0.941 for 1-Hop, 0.815 for 2-Hop, and 0.520 for 3-Hop, outperforming RAG on Gemini generated documents (RAG-Gen) across all hop levels (e.g., RAG-Gen Top-3: 0.784, 0.179, 0.180).

**GNN-RAG: Graph Neural Retrieval for Efficient Large Language Model Reasoning on Knowledge Graphs**
https://doi.org/10.18653/v1/2025.findings-acl.856

- [D101] (supports) Most recent approaches to Knowledge Graph Question Answering (KGQA) rely on costly LLM calls to generate executable relation paths or traverse the KG, which is inefficient in complex KGQA tasks, such as those involving multi-hop or multi-entity questions.

**Statistically Validated Networks in Bipartite Complex Systems**
https://doi.org/10.1371/journal.pone.0017994

- [D104] (supports) The Bonferroni network of organisms comprises 58 non-isolated nodes connected by 216 weighted links and consists of seven connected components, each with a biological interpretation based on organisms' lineage.

**Scale-free networks are rare**
https://doi.org/10.1038/s41467-019-08746-5

- [D107] (supports) Anna D. Broido and Aaron Clauset found robust evidence that strongly scale-free structure is empirically rare across nearly 1000 social, biological, technological, transportation, and information networks, finding that log-normal distributions fit the data as well or better than power laws for most networks.
- [D108] (supports) Different notions of evidence for scale-free structure found in the literature can be organized into a nearly nested set of categories and assessed by applying standard statistical tools to each graph associated with a network data set.
- [D109] (supports) A claim that a network is scale-free should be established using a severe statistical test that goes beyond static degree distributions.
- [D110] (supports) In an evaluation of nearly 1000 real-world networks, fewer than 36 networks (4%) exhibited the strongest level of evidence for scale-free structure, defined as every degree distribution associated with a network being convincingly scale-free.
- [D111] (supports) The procedure for identifying scale-free structure in a network data set involves applying standard statistical methods to each associated simple graph to (1) identify the best-fitting power law in the degree distribution’s upper tail, (2) evaluate its statistical plausibility using a goodness-of-fit test, and (3) compare it to four alternative distributions fitted to the same part of the upper tail using a likelihood-ratio test.
- [D113] (supports) In a study of networks, 49% of networks show no evidence, direct or indirect, of scale-free structure, and in 88% of networks, a log-normal fits the degree distribution as well as or better than a power law.

**arXiv:cond-mat/0008064v1  [cond-mat.dis-nn]  3 Aug 2000**
https://doi.org/10.1038/35019019

- [D112] (supports) Scale-free networks are extremely vulnerable to attacks, which are defined as the selection and removal of a few nodes that play the most important role in assuring the network's connectivity.
- [D114] (supports) Scale-free networks, which describe systems such as the World Wide Web (www), the Internet, social networks, or a cell, display an unexpected degree of robustness where the ability of nodes to communicate remains unaffected even by unrealistically high failure rates.

**error=cookies_not_supported&code=55108452-9215-4aed-b8d4-ffb7ea60552a**
https://doi.org/10.1186/1753-4631-1-3

- [D116] (supports) The clustering coefficient of a vertex is the likelihood that its neighbours are connected.
- [D117] (supports) The small-world phenomenon is characterized by networks with a small fraction of randomly rewired connections that combine both high clustering and a small path length.
- [D120] (supports) Scale-free networks can have very small path lengths of the order of lnln(N), and their clustering coefficient may be smaller than that of small-world networks.
- [D121] (supports) In the Watts and Strogatz model, small-world networks arise for small values of rewiring probability p, combining the high clustering coefficient of ordered networks with the short pathlength of random networks.
- [D122] (supports) Van den Berg and van Leeuwen showed that sparsely connected random graphs above a certain size always give rise to a small-world network.

### Auto-matched facts

These facts were retrieved automatically from the knowledge base for additional auditability against the broad corpus. Each [N] marker in the body links to a fact below.

**Atomic Fact Decomposition Helps Attributed Question Answering**
https://arxiv.org/abs/2410.16708

- [1] (related) The Atomic fact decomposition-based Retrieval and Editing (ARE) framework leverages a search engine to retrieve evidence related to atomic facts and inputs these evidences into an LLM-based verifier to determine whether the facts require expansion for re-retrieval or editing.
- [9] (related) In the Atomic fact decomposition-based Retrieval and Editing (ARE) framework, edited facts are backtracked into the original answer, and evidence is aggregated based on the relationship between molecular clauses and atomic facts.

**Simple Is Effective: The Roles of Graphs and Large Language Models in Knowledge-Graph-Based Retrieval-Augmented Generation**
https://arxiv.org/abs/2410.20724

- [2] (related) Knowledge Graph (KG)-based Retrieval-Augmented Generation (RAG) addresses hallucinations and outdated knowledge in Large Language Models (LLMs) by grounding LLM outputs in structured external knowledge from KGs.

**Meta-validation of bipartite network projections**
https://doi.org/10.1038/s42005-022-00856-9

- [3] (supports) The co-occurrences of nodes i and j of set L in a bipartite network are given by Cij = ∑ α∈Γ MiαMjα, where the L × L square matrix C represents a monopartite network obtained as the projection of the original bipartite network onto the set L (L-projection).
- [19] (related) A bipartite network can be projected onto the set Γ to obtain the co-occurrences between nodes of that set, known as a Γ-projection.
- [20] (related) A bipartite network can be projected onto the set Γ to obtain the co-occurrences between nodes of that set, known as a Γ-projection.
- [22] (related) After defining a binary bipartite matrix, a projected network of co-occurrences between scientific fields i and j is built using the connection formula Cij = ∑αMiαMjα.
- [24] (supports) The approach of projecting BiCM-generated networks allows for the numerical and analytical computation of co-occurrences distributions; this approach simplifies to a Bipartite Partial Configuration Model (BiPCM) when degree constraints are imposed on only one set of nodes.
- [25] (supports) The null model for network projections can be obtained by projecting networks generated by the Bipartite Configuration Model (BiCM).
- [26] (related) The proposed methodology to reconcile four validation schemes involves finding a coherence area in the parameter space where filtered networks show relatively good agreement, beginning with an assessment of structural similarity using three popular metrics of graph distance.
- [84] (related) When modularity is high enough, a robust community structure shared among the null models emerges.
- [85] (related) A community structure is broadly defined as sets of nodes, typically non-overlapping, that possess significantly more internal links (connecting nodes within the same community) than external links (connecting nodes of different communities).
- [86] (related) The Louvain method partition points used for measuring modularity as a function of density (ρ) correspond to the partition of highest modularity obtained in 100 runs of the algorithm with random initialization.
- [99] (related) The standard approach to identify significant links in monopartite projections of bipartite networks is statistical validation using a suitable null network model, such as the configuration model (CM) that constrains node degrees and randomizes all other aspects.
- [100] (related) The null hypothesis corresponding to the bipartite Configuration Model (CM) is that scientific fields are independent and no capability structure exists behind the network, meaning co-occurrences between fields happen at random based on field ubiquities or country diversification.
- [102] (related) Regarding null model validation, the BiCM model validates the least due to having longer tails and a higher mean, whereas the Hypergeometric model validates the most due to having shorter tails and a lower mean.
- [103] (related) The four null models used in the study lead to different results for the density of validated links even when the significance threshold p* is the same.

**Dense X Retrieval: What Retrieval Granularity Should We Use?**
https://arxiv.org/abs/2312.06648

- [4] (supports) The authors of 'Dense X Retrieval: What Retrieval Granularity Should We Use?' introduced a novel retrieval unit for dense retrieval called the 'proposition'.

**Guidance on Substantiation for Dietary Supplement Claims**
https://www.fda.gov/regulatory-information/search-fda-guidance-documents/guidance-industry-substantiation-dietary-supplement-claims-made-under-section-403r-6-federal-food

- [5] (supports) While there is no general rule for the number or combination of types of evidence sufficient to support a claim, the replication of research results in independently conducted studies increases the likelihood that the totality of the evidence will support a claim.
- [6] (supports) Regarding the substantiation of claims, there is no general rule for how many studies or what combination of types of evidence is sufficient, but the replication of research results in independently conducted studies increases the likelihood that the totality of evidence will support a claim.

**Comparative Evaluation of Advanced Chunking for Retrieval-Augmented Generation in Large Language Models for Clinical Decision Support**
https://doi.org/10.3390/bioengineering12111194

- [7] (supports) According to Table 1, the Adaptive chunking strategy achieved a mean accuracy of 2.37 [95% CI 2.10–2.60], a mean relevance of 2.90 [2.73–3.00], a precision of 0.50 [0.31–0.68], a recall of 0.87 [0.69–1.00], and an F1 score of 0.63 [0.36–0.78].
- [27] (related) Proposition-based chunking indexes atomic, claim-level statements to enable high-precision retrieval.
- [28] (related) Proposition-based chunking indexes atomic, claim-level statements for high-precision retrieval using an LLM to extract atomic propositions per sentence (with temperature ≈ 0.2 and max ≈ 256 tokens), which are grouped into chunks until a topic shift occurs or a capacity of approximately 500 words is reached.
- [32] (supports) In the study 'Comparative Evaluation of Advanced Chunking for Retrieval-Augmented Generation in Large Language Models for Clinical Decision Support' (published in Bioengineering 2025, 12, 1194), the Proposition chunking strategy achieved a mean Accuracy of 2.07 [95% CI 1.80–2.33], a mean Relevance of 2.80 [95% CI 2.57–2.97], and IR metrics consisting of a Precision of 0.38 [0.21–0.57], Recall of 0.71 [0.46–0.93], and F1 score of 0.49 [0.25–0.67].
- [104] (related) The adaptive chunking method used in the comparative evaluation of advanced chunking for Retrieval-Augmented Generation (RAG) in Large Language Models for Clinical Decision Support is defined as a flexible policy combining similarity thresholds and variable window sizes to preserve topic continuity while avoiding information dilution.
- [105] (related) The adaptive chunking approach produced chunks of varying size that maintained high internal coherence, where sections of text on a consistent topic formed longer chunks up to the length cap and rapid topic changes triggered smaller chunks, aligning chunk boundaries with shifts in content.

**Association of Ultraprocessed Foods With Mortality Risk Among French Adults**
https://doi.org/10.1001/jamainternmed.2018.7289

- [8] (supports) The authors of the study state that overall caution is warranted when generalizing the results of their research.
- [67] (supports) The authors conclude that no causality can be established for the observed associations in their study.

**Fact in Fragments: Deconstructing Complex Claims via LLM-based Atomic Fact Extraction and Verification**
https://arxiv.org/abs/2506.07446

- [10] (related) The Atomic Fact Extraction and Verification (AFEV) framework dynamically refines claim understanding and reduces error propagation through iterative fact extraction, reranks evidence to filter noise, and leverages context-specific demonstrations to guide the reasoning process.

**Pseudo-Knowledge Graph: Meta-Path Guided Retrieval and In-Graph Text for RAG-Equipped LLM**
https://arxiv.org/html/2503.00309v1

- [11] (supports) The PKG Builder uses a hybrid approach to construct the PKG by integrating traditional Natural Language Processing (NLP) algorithms with advanced language model techniques to enhance entity recognition and relation extraction.
- [12] (supports) The PKG Builder uses a hybrid approach combining Large Language Models (LLMs) and traditional NLP techniques to ensure robust and accurate entity and relation extraction.
- [13] (related) The Pseudo-Knowledge Graph (PKG) framework consists of two core components: the PKG Builder and the PKG Retriever.
- [33] (supports) The MultiHop-RAG dataset consists of news articles published between September 2013 and December 2023, covering categories including entertainment, business, sports, technology, health, and science.
- [34] (supports) Yixuan Tang and Yi Yang published 'MultiHop-RAG: Benchmarking retrieval-augmented generation for multi-hop queries' as arXiv:2401.15391 [cs.CL] in 2024.
- [35] (related) MultiHop-RAG includes Inference Queries, which require a model to perform multi-hop reasoning by connecting information from different articles to deduce a correct answer, testing the model's ability to integrate and reason over multiple pieces of information.
- [41] (related) MultiHop-RAG focuses on structured, multi-hop reasoning, requiring models to synthesize information across multiple documents and perform complex inference.
- [43] (related) The PKG Builder's methodology consists of two main steps: (1) applying NLP algorithms to identify entities and relations to convert raw data into a structured format, and (2) refining the extraction process using language models.
- [45] (supports) In relation extraction, rule-based patterns defined using syntactic structures or specific phrases are applied to detect relations.
- [49] (related) Large Language Models (LLMs) integrated with Knowledge Graphs (KGs) outperform those using Retrieval-Augmented Generation (RAG) in reasoning and understanding because KGs provide richer context and a more nuanced understanding of how information interrelates, enhancing the model's ability to interpret complex scenarios.
- [50] (supports) The LLM with PKG method integrates recent, specific data with structured knowledge using natural language and node-relation chains through meta-paths to deliver well-rounded and accurate answers.
- [51] (supports) The Pseudo-Knowledge Graph (PKG) uses a hybrid approach of combining structured graph data with unstructured text to allow large language models (LLMs) to efficiently interpret and utilize retrieved information, which helps the models bypass typical difficulties associated with purely structured data formats.
- [52] (supports) The superior performance of PKG is attributed to three factors: (i) providing LLMs with rich information through PKG by leveraging diverse retrieval methods, which results in a wider variety of information types and higher quality data; (ii) retaining original text chunks within the PKG, allowing LLMs to bypass the complexities of processing structured data and work with familiar unstructured text formats; and (iii) utilizing meta-paths to perform complex relationship analyses, which enhances the method's performance in understanding and reasoning tasks by allowing the model to discern intricate patterns and connections.
- [62] (supports) Knowledge Graphs (KGs) are structured representations of knowledge that capture relationships between entities in a graph format.
- [63] (related) Traditional Knowledge Graphs (KGs) represent information in a structured format using nodes and edges to capture relationships between entities.
- [64] (supports) Traditional Retrieval-Augmented Generation (RAG) systems often struggle to discern and leverage the relationships between different pieces of information.
- [66] (supports) Retrieval-Augmented Generation (RAG) struggles with comprehensive retrieval in high-volume, low-information-density databases and lacks relational awareness, which leads to fragmented answers.
- [77] (related) Large language models (LLMs) cannot inherently verify the truthfulness of their outputs, which necessitates the extraction of third-party facts to support their claims.
- [80] (related) The complexity of the MultiHop-RAG dataset makes it a benchmark for evaluating the advanced reasoning and retrieval capabilities of language models.
- [87] (related) Multiple supporting facts are required to ensure the reliability of retrieved information in the context of Retrieval-Augmented Generation (RAG).
- [90] (related) Due to the limited ability of Large Language Models (LLMs) to understand structured data, Knowledge Graphs (KGs) do not perform as well as vector database-based Retrieval-Augmented Generation (RAG) for tasks that do not require strong logical reasoning.
- [93] (related) For entity extraction, the authors use traditional NLP methods, including handcrafted rules, regular expressions, and linguistic cues, which are precise in well-defined contexts but require domain-specific knowledge.
- [98] (related) The researchers selected Open Compass (Buitrago and Nystrom, 2019) and MultiHop-RAG (Tang and Yang, 2024), two datasets comprising approximately one million tokens, to represent diverse corpora encountered in real-world scenarios.
- [107] (related) Knowledge graphs provide structured information by capturing relationships and facts to ensure accuracy and coherence.
- [109] (supports) The Pseudo-Knowledge Graph (PKG) utilizes vector-based retrieval for semantic similarity and meta-path retrieval for uncovering complex, multi-hop relationships, such as 'author-paper-conference' or 'disease-symptom-treatment'.
- [110] (supports) Building on the Retrieval-Augmented Generation (RAG) paradigm, the Pseudo-Knowledge Graph (PKG) integrates knowledge graphs, meta-path retrieval, and natural language text preservation to create a context-aware retrieval system.

**SG-RAG MOT: SubGraph Retrieval Augmented Generation with Merging and Ordering Triplets for Knowledge Graph Multi-Hop Question Answering**
https://www.mdpi.com/2504-4990/7/3/74

- [14] (supports) Failure cases for SG-RAG MOT include instances where there is a high repetition of the 'Brad Bird' entity in the retrieved triplets, and instances where there is a high number of retrieved triplets.

**Grounded Knowledge Graph Extraction via LLMs: An Anchor-Constrained Framework with Provenance Tracking**
https://doi.org/10.3390/computers15030178

- [15] (related) The smaller relation vocabulary of Wiki-NRE compared with REBEL allowed the authors to examine whether the benefits of AEVS generalize across different levels of schema complexity.
- [16] (supports) In the AEVS analysis, the REBEL dataset (200 relation types) had the highest input cost at 14,037–16,867 tokens/sample, followed by WebNLG (159 types) at 9,694–12,014 tokens/sample, and Wiki-NRE (45 types) at 3,754–4,618 tokens/sample.
- [18] (related) The anchor set in the Anchor Discovery process establishes a closed vocabulary of elements for triplets; any triplet element that cannot be traced back to an anchor indicates a potential hallucination.
- [30] (related) The AEVS framework uses semantic deduplication with sentence embeddings to prevent redundant triplets across extraction rounds, which maintains precision while improving recall.
- [36] (contradicts) Adding anchor-based extraction resulted in the most substantial improvement in the system, which validates that text-grounded constraints are effective in reducing hallucinated outputs.
- [38] (supports) Partial matching is an evaluation metric that computes token overlap between candidate and reference triplets without requiring exact correspondence, providing a measure of semantic overlap that is lenient toward minor surface-form variations.
- [39] (related) Exact matching is an evaluation metric requiring complete token-level matching between candidate and reference triplets, providing an unambiguous measure of extraction quality for applications requiring exact schema conformance.
- [40] (related) Hallucination rates across models and datasets (Figure 17) span two orders of magnitude, ranging from 0.23% for Claude on WebNLG to 20.23% for GPT-4o-mini on Wiki-NRE.
- [54] (related) AEVS uses a flexible anchor discovery mechanism that accommodates diverse surface forms, including literals, by treating all salient text spans as potential anchors without imposing rigid type constraints.
- [72] (related) In the AEVS framework, each triplet element (subject, relation, object) receives a binary grounding status indicating whether it was successfully linked to source evidence, an architectural constraint that limits the model’s ability to fabricate information beyond what the text contains.

**Detecting hallucinations in large language models using semantic entropy - Nature**
https://doi.org/10.1038/s41586-024-07421-0

- [17] (related) Hallucinations are a critical problem for natural language generation systems using large language models (LLMs), such as ChatGPT or Gemini, because users cannot trust that any given output is correct.
- [29] (related) The researchers decompose paragraphs into factual claims using the following prompt: "Please list the specific factual propositions included in the answer above. Be complete and do not leave any factual claims out. Provide each claim as a separate sentence in a separate bullet point."
- [42] (related) Rejection accuracy is the accuracy of the answers of the model on the remaining questions after some have been refused, and the AURAC is a summary statistic of this accuracy over many thresholds.
- [96] (related) Naive estimates of entropy or other lexical variation scores can be misleadingly high when the same correct answer is written in multiple ways without changing its meaning.
- [97] (related) Naive estimates of entropy or lexical variation scores can be misleadingly high in free-form generation when the same correct answer is written in many different ways without changing its meaning.

**Statistically Validated Networks in Bipartite Complex Systems**
https://doi.org/10.1371/journal.pone.0017994

- [21] (related) The properties of bipartite complex systems are frequently investigated by analyzing the one-mode projection of the bipartite network.
- [23] (related) In a bipartite system S, the adjacency projected network on set A is obtained by linking together vertices of A that share at least one common first neighbor element of set B.
- [46] (supports) The statistically validated network depends on the way the statistical threshold is set.
- [47] (related) In the Bonferroni network associated with a system of 500 stocks, the nodes represent stocks and links connecting different stocks correspond to statistically validated relationships.
- [48] (related) The FDR network of the system includes 494 stocks and 11,281 multi-links, which is more than the Bonferroni network because the requirement for statistical validation is less restrictive.
- [81] (related) The procedure for the construction of a statistically validated network consists of the following steps: (1) set a value of statistical significance, (2) compare each p-value with the statistical threshold, (3) validate the link between elements for a specific subsystem S if the p-value meets the threshold, (4) summarize all validations in a projected adjacency network and assign a weight to the link between elements equal to the total number of subsystems S in which the relationship was statistically validated, (5) remove any link with a weight of zero.
- [106] (related) A one-mode projection of a bipartite network is created by forming a network of nodes belonging to one of the two sets, where two nodes are connected if they share at least one common neighboring node from the other set.

**From Local to Global: A Graph RAG Approach to Query-Focused Summarization**
https://arxiv.org/abs/2404.16130

- [31] (related) The GraphRAG approach uses a large language model (LLM) to build a graph index in two stages: (1) derive an entity knowledge graph from the source documents, and (2) pregenerate community summaries for all groups of closely related entities.
- [89] (supports) For a class of global sensemaking questions over datasets in the 1 million token range, GraphRAG leads to substantial improvements over a conventional RAG baseline in both the comprehensiveness and diversity of generated answers.
- [91] (related) GraphRAG is a proposed graph-based approach to question answering over private text corpora that scales with both the generality of user questions and the quantity of source text, combining the strengths of RAG and QFS methods.

**Self-RAG: Learning to Retrieve, Generate, and Critique through Self-Reflection**
https://arxiv.org/abs/2310.11511

- [37] (supports) Retrieval-Augmented Generation (RAG) is an ad hoc approach that decreases factual inaccuracies in language models by augmenting them with the retrieval of relevant knowledge.

**Scalable Zero-shot Entity Linking with Dense Entity Retrieval**
https://doi.org/10.18653/v1/2020.emnlp-main.519

- [44] (related) The authors assume each mention has a valid gold entity in the knowledge base, a process referred to as in-KB evaluation.
- [56] (related) Khalife and Vazirgiannis (2018) reported 94.57% precision on the TACKBP-2010 dataset, although their method assumes that a gold fine-grained entity type is provided for each mention.
- [88] (related) In two-stage entity linking systems, the choice of the number of candidates retrieved (k) influences the overall model performance.

**YAGO: A Large Ontology from Wikipedia and WordNet**
https://doi.org/10.1016/j.websem.2008.06.001

- [53] (supports) A high-quality ontology with accuracy close to 100% (comparable to an encyclopedia) that integrates knowledge from several sources could boost the performance of existing applications.
- [61] (supports) Because systems that extract knowledge structures from text often use non-canonical forms, no explicit logic-based knowledge representation model is available for them.

**Autoregressive Entity Retrieval**
https://arxiv.org/abs/2010.00904

- [55] (related) The authors of the research demonstrate that new entities can be added to their system by simply specifying their names.

**Think-on-Graph: Deep and Responsible Reasoning of Large Language Model on Knowledge Graph**
https://arxiv.org/abs/2307.07697

- [57] (related) Introducing external knowledge graphs (KG) in large language model (LLM) reasoning could partially address hallucination problems.

**A Survey on Hallucination in Large Language Models: Principles, Taxonomy, Challenges, and Open Questions**
https://arxiv.org/abs/2311.05232

- [58] (related) Large language models (LLMs) are prone to hallucination, which involves generating content that is plausible but nonfactual.

**A Comprehensive Survey of Hallucination Mitigation Techniques in Large Language Models**
https://arxiv.org/abs/2401.01313

- [59] (related) Large Language Models (LLMs) have a tendency to hallucinate, which is the generation of content that appears factual but is ungrounded.

**Factuality challenges in the era of large language models and opportunities for fact-checking - Nature Machine Intelligence**
https://doi.org/10.1038/s42256-024-00881-z

- [60] (related) Large language models (LLMs) tend to produce false, erroneous, or misleading content, a phenomenon commonly referred to as hallucinations.

**LightRAG: Simple and Fast Retrieval-Augmented Generation**
https://arxiv.org/abs/2410.05779

- [65] (supports) Existing Retrieval-Augmented Generation (RAG) systems have significant limitations, including reliance on flat data representations and inadequate contextual awareness, which can lead to fragmented answers that fail to capture complex inter-dependencies.

**Applying the Bradford Hill criteria in the 21st century: how data integration has changed causal inference in molecular epidemiology - Discover Public Health**
https://doi.org/10.1186/s12982-015-0037-4

- [68] (supports) The Bradford Hill Criteria should be viewed as a list of possible considerations to generate thoughtful discourse among researchers from diverse scientific fields, rather than used as a heuristic for assessing causation in a vacuum.
- [69] (supports) Statistically significant results in a study are not always biologically meaningful or methodologically appropriate for contributing to causal inference.

**Convolutional 2D Knowledge Graph Embeddings**
https://doi.org/10.1609/aaai.v32i1.11573

- [70] (supports) Link prediction for knowledge graphs is the task of predicting missing relationships between entities.

**Learning Entity and Relation Embeddings for Knowledge Graph Completion**
https://doi.org/10.1609/aaai.v29i1.9491

- [71] (related) In the paper 'Learning Entity and Relation Embeddings for Knowledge Graph Completion', the authors evaluate their models on three tasks: link prediction, triple classification, and relational fact extraction.

**Wikidata: A Free Collaborative Knowledgebase**
https://cacm.acm.org/research/wikidata/

- [73] (supports) Wikidata allows conflicting data to coexist and provides mechanisms to organize this plurality.

**Improving Evidence Retrieval for Automated Explainable Fact-Checking**
https://doi.org/10.18653/v1/2021.naacl-demos.10

- [74] (related) The context-aware sentence selection model uses the BIO tagging format for output, where irrelevant tokens are classified as O, the first token of an evidence sentence is classified as B evidence, and the remaining tokens of an evidence sentence are classified as I evidence.
- [75] (related) The selection of relevant evidence sentences for accurate fact-checking and explainability remains a challenge.

**Synchronous Faithfulness Monitoring for Trustworthy Retrieval-Augmented Generation**
https://doi.org/10.18653/v1/2024.emnlp-main.527

- [76] (related) Retrieval-augmented language models (RALMs) have significant trustworthiness concerns because they are prone to generating unfaithful outputs, including baseless information or contradictions with the retrieved context.

**AttributionBench: How Hard is Automatic Attribution Evaluation?**
https://arxiv.org/abs/2402.15089

- [78] (supports) Evaluating the attribution of an answer—specifically whether every claim within generated responses is fully supported by its cited evidence—remains an open problem.

**LSHBloom: Memory-efficient, Extreme-scale Document Deduplication**
https://arxiv.org/abs/2411.04257

- [79] (related) Deduplication, which is the detection and elimination of additional instances of the same content, is a major focus for assembling and curating training datasets for large language models (LLMs).

**error=cookies_not_supported&code=55108452-9215-4aed-b8d4-ffb7ea60552a**
https://doi.org/10.1186/1753-4631-1-3

- [82] (supports) Following thresholding, unweighted, undirected graphs were characterized using graph theoretical measures including clustering coefficient, path length, small world metric σ (defined as [C/C-random] / [L/L-random]), clustering, characteristic length scale, betweenness, and synchronizability (likely referring to the eigenvalue ratio λN/λ2 based on graph spectral analysis).
- [83] (supports) The unweighted, undirected graphs in the study by Bassett et al. were characterized using measures including clustering coefficient, path length, small world metric σ ([C/C-random]/[L/L-random]), clustering, characteristic length scale, betweenness, and synchronizability (likely referring to the eigenvalue ratio λN/λ2 based upon graph spectral analysis).

**Scale-free networks are rare**
https://doi.org/10.1038/s41467-019-08746-5

- [92] (related) The study's domain-specific analysis focused on networks from biological, social, and technological sources, which together comprise 91% of the corpus.
- [94] (supports) The log-normal distribution is at least as good a fit as the power law for 88% of degree distributions, suggesting that many previously identified scale-free networks may actually be log-normal networks.
- [95] (supports) Across the studied corpus, likelihood ratio tests find only modest support for the power-law distribution over four non-power-law alternatives.

**GNN-RAG: Graph Neural Retrieval for Efficient Large Language Model Reasoning on Knowledge Graphs**
https://doi.org/10.18653/v1/2025.findings-acl.856

- [101] (supports) On multi-hop and multi-entity questions, GNN-RAG outperforms LLM-based retrieval approaches by 8.9–15.5% points at answer F1.

**Unifying Large Language Models and Knowledge Graphs: A Roadmap**
https://arxiv.org/abs/2306.08302

- [108] (related) Knowledge Graphs (KGs), such as Wikipedia and Huapu, are structured knowledge models designed to explicitly store rich factual knowledge.

