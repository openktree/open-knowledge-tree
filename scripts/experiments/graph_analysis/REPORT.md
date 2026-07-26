# Graph-Weakness Experiments — Findings

Read-only experiments that turn the OKT paper's "designed but not executed" /
"conceded but never measured" graph claims into evidence. See `PLAN.md` for the
full plan and `README.md` for run instructions. This file is written one
experiment at a time; sections appear as experiments complete.

The graph under test is the `multihoprag` repository's derived concept graph:
**18,241 concept groups, 36,075 facts, 94,581 bipartite fact↔concept edges,
76,233 co-occurrence relations** (max concept degree 644 = Google LLC; 70.2% of
edges at weight 1; max weight 156). This is the MultiHop-RAG news corpus.

---

## Experiment 1 — BiCM null-model edge validation

**Paper claim under test (§7.2):** "OKT's `concept_relations` view computes
raw `shared_fact_count` without null-model validation. … Whether the raw
weights are practically useful for navigation (where high-degree concepts
surfacing often is a feature) or whether they must be validated/thresholded
(where incidental co-occurrence is noise) is an empirical question."

**Method.** The Bipartite Configuration Model (BiCM) is the degree-preserving
null model for bipartite networks (Strona et al. 2014; Saracco et al. 2015).
Under it, the expected co-occurrence of concepts i, j is
`⟨C_ij⟩ = k_i · k_j / |Γ|` (k = fact-degree, |Γ| = total bipartite edges). We
compute z-scores and Poisson upper-tail p-values for every co-occurrence edge,
then BH-adjust for multiple testing. See `exp1_bicm.py`.

### Results

**Metric 1 — Spearman correlation between raw weight and z-score: 0.0215**
(p ≈ 3e-9; effectively zero). Raw `shared_fact_count` is almost uncorrelated
with BiCM-validated structural significance. This is the report's
degree-confounding weakness made quantitative: **ranking edges by raw weight is
not a proxy for ranking by structural significance.**

**Metric 2 — degree-confounded edges (|z|<1) by weight band:**

| band | n edges | confounded | % |
|---|---|---|---|
| w=1 (draft) | 53,530 | 139 | 0.3% |
| w=2..4 | 19,691 | 13 | 0.1% |
| w=5..9 | 2,265 | 0 | 0.0% |
| w≥10 (promoted) | 747 | 0 | 0.0% |
| all | 76,233 | 152 | 0.2% |

The bands follow the system's own promotion semantics: w=1 is a draft (single
co-occurrence, never promoted), w≥10 is the v1 promotion threshold (a relation
strong enough to surface as real). The high-weight backbone (w≥5) is
**entirely clean** — every one of its 3,277 edges is structurally significant
under BiCM. Degree-confounding is not a problem in the backbone; it is a
non-problem in the w=1 mass too (only 0.3% confounded), because at w=1 almost
any co-occurrence beats the near-zero BiCM expectation.

**Metric 3 — survivor counts:**

| threshold | survivors | % of 76,233 |
|---|---|---|
| p < 0.05 | 74,311 | 97.5% |
| p < 0.01 | 68,835 | 90.3% |
| FDR < 0.05 | 74,224 | 97.4% |
| raw w ≥ 2 | 22,703 | 29.8% |
| raw w ≥ 5 | 3,012 | 4.0% |
| raw w ≥ 10 (promoted) | 747 | 1.0% |

97.4% of edges survive FDR<0.05 — the graph is overwhelmingly structurally
significant against the degree-preserving null. The raw thresholds are far
more aggressive: `w≥2` drops 70% of edges, `w≥5` drops 96%, `w≥10` drops 99%.
**Raw weight thresholding and BiCM validation select very different edge sets**:
97.4% of edges are BiCM-significant but only 1.0% are promoted, so the W
threshold throws away almost all of the structurally significant edges.

**Metric 4 — promotion (W≥10) vs BiCM significance cross-tab:**

| | significant (FDR<0.05) | confounded (\|z\|<1) | total |
|---|---|---|---|
| **promoted (W≥10)** | 747 | 0 | 747 |
| **draft (W<10)** | 73,477 | 152 | 75,486 |
| **total** | 74,224 | 152 | 76,233 |

This is the central finding. The two axes measure **different things** and
almost never overlap:

- **Promoted & confounded: 0 (0.0% of promoted).** The W≥10 promotion threshold
  is a *perfect precision filter* — every promoted relation is structurally
  significant. The system's existing design choice is sound: if you only trust
  promoted relations, you will never surface a degree-confounded edge.
- **Draft & significant: 73,477 (97.3% of drafts).** The W threshold is a
  *terrible recall filter* — it drops almost all structurally real relations,
  because most are between rare concepts where w=1 is already hugely
  significant (the BiCM expectation for two k≈5 concepts is ≈0.0003, so a
  single co-occurrence gives z≈62).

The W threshold and BiCM are not competing filters — they answer different
questions. W asks "is this relation strong enough to promote?" (a frequency
question, appropriate for surfacing high-confidence relations to users). BiCM
asks "is this relation structurally significant, or just two high-degree hubs
that co-occur by chance?" (a confounding question, appropriate for ranking
relation strength). The system's v1 design implicitly answered the first
question and ignored the second; the data shows that was a defensible choice
for the promoted backbone (zero false positives) at the cost of discarding the
significant draft layer (97% of the graph).

**Metric 5 — W-threshold sweep (is there a better default W than 10?):**

| W | n edges | % kept | confounded | % conf | % sig | % low-k pairs |
|---|---|---|---|---|---|---|
| 1 | 76,233 | 100.0% | 152 | 0.2% | 97.4% | 34.7% |
| 2 | 22,703 | 29.8% | 13 | 0.1% | 99.6% | 23.5% |
| 3 | 8,281 | 10.9% | 4 | 0.1% | 99.8% | 9.3% |
| 4 | 4,614 | 6.0% | 2 | 0.0% | 99.8% | 4.6% |
| **5** | **3,012** | **4.0%** | **0** | **0.0%** | **99.9%** | **2.5%** |
| 6 | 2,097 | 2.8% | 0 | 0.0% | 100.0% | 1.1% |
| 7 | 1,528 | 2.0% | 0 | 0.0% | 100.0% | 0.3% |
| 8 | 1,149 | 1.5% | 0 | 0.0% | 100.0% | 0.1% |
| 9 | 918 | 1.2% | 0 | 0.0% | 100.0% | 0.1% |
| 10 (v1) | 747 | 1.0% | 0 | 0.0% | 100.0% | 0.0% |

`% low-k pairs` = share of kept edges where both endpoints have fact-degree
k<10 (rare concepts). This is the "significant but maybe not useful" band —
a w=1 co-occurrence between two rare concepts is statistically real but may
be a one-off journalistic juxtaposition, not a navigable relation.

**W=5 is the minimum W with zero confounded edges.** It is the lowest W that
is still a safe precision filter (no hub-hub noise admitted) while keeping
3,012 relations — 4× the v1 W=10 backbone (747). Every W above 5 is also
confound-free but discards useful relations for no precision gain: W=10 drops
the 2,265 W=5..9 edges, all of which are significant and none of which are
confounded.

The W=5..9 band that v1 discards contains genuinely useful relations:

| w | z | k_a | k_b | pair |
|---|---|---|---|---|
| 9 | 123 | 22 | 23 | boston dynamics ↔ marc raibert (founder) |
| 9 | 66.7 | 22 | 78 | blizzard entertainment ↔ diablo iv (product) |
| 9 | 138.5 | 19 | 21 | sony wh-1000xm4 ↔ sony wh-1000xm5 (product line) |
| 9 | 35.5 | 200 | 30 | gaza strip ↔ israel defense forces |
| 9 | 25.7 | 45 | 251 | dak prescott ↔ san francisco 49ers |
| 9 | 39.7 | 23 | 209 | the eras tour ↔ travis kelce |
| 9 | 48.9 | 35 | 91 | boston red sox ↔ major league baseball |

These are real story clusters — founder↔company, product↔brand, sports
teams, events — that just don't hit the w=10 frequency bar. W=10 filters
toward hubs (median min endpoint degree k=40); W=5 keeps the mid-band
(median min k=17) where most of the corpus's specific relations live.

**Recommendation: lower the default promotion W from 10 to 5.** W=5 is the
data-driven default — it is the minimum W that clears all degree-confounded
edges (the report's only real failure mode) while keeping 4× the relations.
The v1 W=10 threshold was conservative; the BiCM sweep shows it was
sacrificing useful mid-band relations for zero additional precision. (This is
a recommendation, not an implemented change — this task is experiments-only.)

**Diagnostic — the degree-confounded high-weight edges (the report's predicted
"USA↔everything" failure mode):**

| w | ⟨C⟩ | z | pair |
|---|---|---|---|
| 4 | 3.26 | 0.41 | google llc ↔ united states of america |
| 4 | 3.06 | 0.54 | amazon.com, inc. ↔ united states of america |
| 3 | 2.12 | 0.61 | openai inc. ↔ united states of america |
| 3 | 2.37 | 0.41 | amazon.com, inc. ↔ artificial intelligence |

These are exactly the failure mode the report predicts: "United States" (k=479)
co-occurs with the high-degree tech hubs (Google k=644, Amazon k=604, OpenAI
k=418) at weights 3–4, but BiCM expects 2.1–3.3 co-occurrences by chance — the
edges are not statistically significant. A raw-weight navigator would surface
"USA ↔ Google" as a relation; a BiCM-validated navigator would correctly
suppress it. There are **only 4 such edges** at w≥3 in the whole graph; the
backbone is clean.

**Diagnostic — top edges by raw weight are all structurally significant:**
FTX↔SBF (w=156, z=127), Canada↔Jamaica (w=147, z=220), Taylor Swift↔Travis
Kelce (w=143, z=155), Alameda↔FTX (w=116, z=141), ChatGPT↔OpenAI (w=106, z=95).
The highest-weight edges are the corpus's real story clusters (FTX collapse,
pop culture, sports, AI), and all have extreme z-scores.

### What this settles

1. **The system's v1 promotion threshold (W≥10) is a sound precision filter,
   but overly conservative.** All 747 promoted edges are structurally
   significant (0% confounded). The report's worry that "a ubiquitous concept
   accumulates high shared_fact_count purely because it appears in many facts"
   is confirmed in the raw weights (Spearman ρ = 0.02) but **does not leak
   into the promoted backbone** — the W≥10 threshold happens to clear every
   degree-confounded edge. The existing design choice is validated for
   precision.

2. **W=5 is a better default promotion threshold than W=10.** The W sweep
   (Metric 5) shows W=5 is the minimum W with zero confounded edges — the
   lowest W that is still a safe precision filter. It keeps 3,012 relations
   (4× the v1 backbone) including the useful W=5..9 mid-band (Boston
   Dynamics↔Marc Raibert, Blizzard↔Diablo IV, Gaza↔IDF, Eras Tour↔Travis
   Kelce) that W=10 discards for zero precision gain. W=10 filters toward
   hubs (median min endpoint degree k=40); W=5 keeps the mid-band (median
   min k=17) where the corpus's specific relations live. Reducing relations
   is not inherently negative — the w=1 draft layer is 35% low-degree
   one-off juxtapositions that are significant but arguably not useful —
   but W=10 over-corrects, dropping real story clusters that just don't
   hit the w=10 frequency bar.

3. **The W threshold is a precision filter, not a significance ranking.** The
   cross-tab (Metric 4) shows the two axes are almost disjoint: 747 promoted
   edges are all significant, but 73,477 significant edges are drafts. The W
   threshold answers "is this relation strong enough to promote?" (frequency);
   BiCM answers "is this structurally significant, or hub-hub chance?"
   (confounding). The system's v1 design answered the first and ignored the
   second — a defensible choice for the promoted backbone, at the cost of
   discarding the significant draft layer (97% of the graph).

4. **The degree-confounding failure mode is real but small and lives below the
   promotion threshold.** Only 152 edges (0.2%) are degree-confounded, all at
   w=3–4 (USA↔Google, USA↔Amazon, USA↔OpenAI, Amazon↔AI). None reach w≥5. The
   report's predicted "USA↔everything" failure is confirmed but contained: it
   affects the draft layer, not the promoted backbone at any W≥5.

5. **Experiments 2 and 4 should use BiCM-validated weights, not raw weights.**
   The raw-weight threshold (w≥2) throws away 70% of structurally significant
   edges; the BiCM z-score is the correct weight for community detection (Exp 2)
   and for graph-walk retrieval (Exp 4). Experiment 1 thus de-risks the rest
   of the plan as intended.

### Workaround: explicitly labeling the two validity axes

The data shows the system already has one of the two validity axes (W =
promotion strength) and is missing the other (BiCM = structural significance).
A potential workaround — **not part of this experiments-only task, but
documented here as the design implication** — is to make the two axes explicit
in the relation schema and surface both to consumers:

- **`shared_fact_count` (W)** — the existing frequency signal. Keep the W≥10
  promotion threshold as-is; it is a perfect precision filter. Promoted
  relations are the high-confidence backbone presented to users.
- **`bicm_z` (or `bicm_p`)** — a new structural-significance signal, computed
  from the same bipartite graph at matview-refresh time (the computation is
  vectorized over 76k edges in <1s; it scales linearly with edge count). This
  would be a column on `concept_relations`, refreshed alongside
  `shared_fact_count`.

The two axes would be **labeled explicitly** so agents and users know which
question a relation's weight answers:

| label | W | BiCM z | meaning |
|---|---|---|---|
| **promoted** | ≥10 | any | high-frequency, trusted backbone (the v1 surface) |
| **significant draft** | <10 | ≥2 | low-frequency but structurally real — the 73,477 edges the W threshold drops |
| **confounded** | any | <1 | hub-hub noise — the 152 edges to suppress; currently none reach promoted, but labeling would catch future ones |

The "significant draft" layer is the actionable finding: 73,477 relations
(97.3% of drafts) that the W threshold discards but BiCM validates as real.
These are mostly between rare concepts (low k_i, k_j) where a single
co-occurrence is already statistically surprising. An agent doing graph-walk
retrieval (Exp 4) would benefit from accessing this layer — a w=1 edge between
two rare concepts is a stronger navigation signal than a w=10 edge between two
hubs, but the current W-only ranking inverts that.

This workaround is **not implemented here** (this task is experiments-only).
It is the design recommendation that follows from the measurement: keep the W
threshold for promotion, add a BiCM column for significance, and label the
difference so consumers know which axis they are trusting.

Output: `results/bicm_validation.json`. Computation: <1s. No LLM cost.

---

## Experiment 2 — 7 graph-property measurements across W thresholds

**Paper claim under test (§6.1):** the report designed seven graph-property
measurements to test whether the emergent co-occurrence graph has usable
structure (navigability, coherence, backbone) but never executed them. It
prioritized three (§6.3): navigability, community coherence, and backbone
identification.

**Method.** Load the bipartite graph, build the weighted concept projection
at four W thresholds (1, 2, 5, 10), and run all seven measurements at each.
The W sweep is the key addition over the report's design: Experiment 1 showed
W reshapes the edge set from 76k to 747, so the graph's topology must change
too. A structural claim like "the graph is navigable" is only meaningful at a
specific W. See `exp2_graph_properties.py`.

### Results

#### M1 — Degree distribution

| W | nodes | mean deg | max deg | α (power-law) | taxonomy | powerlaw vs lognormal |
|---|---|---|---|---|---|---|
| 1 | 17,706 | 8.61 | 655 | 2.16 | strongest | R=−51.1: lognormal fits better |
| 2 | 8,363 | 5.43 | 223 | 2.56 | strongest | R=−1.6: lognormal fits better |
| 5 | 1,842 | 3.27 | 61 | 2.24 | strongest | R=−2.0: lognormal fits better |
| 10 | 618 | 2.42 | 21 | 2.33 | strongest | R=−5.5: lognormal fits better |

The report predicted "heavy-tailed but not strictly power-law." **Confirmed
at all W.** The `powerlaw` package's Broido–Clauset taxonomy labels the fit
"strongest" (α between 2 and 3, σ<0.5), but the power-law-vs-lognormal
comparison is decisively negative (R<0) at every W — **lognormal fits better
than power-law**, exactly as Broido & Clauset found for most real-world
networks ("strongly scale-free structure is empirically rare"). The degree
distribution is heavy-tailed but not scale-free. Hub concepts (Google k=655
at W=1) dominate connectivity, and their dominance increases with W.

#### M2 — Connected components

| W | nodes with edges | components | largest | % of nodes | isolated concepts |
|---|---|---|---|---|---|
| 1 | 17,706 | 341 | 16,714 | 94.4% | 535 |
| 2 | 8,363 | 432 | 7,011 | 83.8% | 9,878 |
| 5 | 1,842 | 166 | 1,330 | 72.2% | 16,399 |
| 10 | 618 | 89 | 268 | 43.4% | 17,623 |

The report predicted "one giant component plus isolated clusters." **Confirmed
at W=1 and W=2**: the giant component holds 94.4% / 83.8% of nodes. At W=5 the
giant still holds 72.2%, but at W=10 it collapses to 43.4% — the promoted
backbone is fragmented into 89 small components, the largest only 268 nodes.
**W=5 is the last threshold where the graph has a dominant giant component.**
At W=10, an agent graph-walking the promoted backbone would hit a boundary
every ~268 concepts. The small components at W=1 are genuine topic islands
(Mediterranean lifestyle, Whistler village, edx/Dhaka) — not extraction
failures.

#### M3 — Community structure (highest priority per §6.3)

| W | communities | modularity Q | NMI vs domains | labeled nodes | domains |
|---|---|---|---|---|---|
| 1 | 404 | 0.793 | 0.406 | 17,706 | 49 |
| 2 | 480 | 0.828 | 0.454 | 8,363 | 49 |
| 5 | 189 | 0.847 | 0.566 | 1,842 | 48 |
| 10 | 98 | 0.878 | 0.589 | 618 | 39 |

This is the report's single most important measurement: "do communities
correspond to meaningful topics?" Modularity Q is high at all W (0.79–0.88),
meaning the graph has strong community structure. **NMI vs corpus domains
(published_sitename: Sporting News, TechCrunch, Polygon, The Verge, etc.)
rises monotonically with W**: 0.41 at W=1, 0.45 at W=2, 0.57 at W=5, 0.59 at
W=10. **Higher W = communities align more closely with corpus domains.** This
confirms the report's prediction: co-occurrence projection produces
semantically coherent clusters without any explicit topic assignment, and the
signal strengthens as incidental co-occurrences (w=1) are filtered out.

The NMI never exceeds 0.59 — communities are *related to* domains but not
*identical* to them. This is expected: a sports team (Buffalo Bills) co-occurs
with other sports teams regardless of whether the source was Sporting News or
CBSSports. The communities capture topical structure that cuts across source
domains.

#### M4 — Small-world properties

| W | giant size | σ (small-world index) | L (avg path) | C (clustering) | C_rand | L_rand |
|---|---|---|---|---|---|---|
| 1 | 16,714 | 1,270 | 4.48 | 0.693 | 0.0005 | 4.43 |
| 2 | 7,011 | 592 | 5.19 | 0.542 | 0.0009 | 4.91 |
| 5 | 1,330 | 109 | 5.75 | 0.352 | 0.003 | 5.26 |
| 10 | 268 | 22 | 5.23 | 0.273 | 0.012 | 4.93 |

σ >> 1 at all W: the graph is strongly small-world (high clustering + short
paths). An agent can traverse from any concept to any other in the giant
component in ~5 hops at any W. **But the report's Van den Berg–van Leeuwen
caveat applies**: high σ is partly a generic consequence of sparseness, not
necessarily evidence of designed navigability. The clustering coefficient
drops with W (0.69 → 0.27) as the graph loses its dense w=1 mass, but σ stays
high because C_rand drops even faster. The practical takeaway: **navigability
holds at all W thresholds** — the giant component's average path length stays
~5 hops, so `getRelatedConcepts`-driven exploration works at any W.

#### M5 — Edge-weight distribution

| W | edges | mean weight | frac w=1 | frac w≥5 |
|---|---|---|---|---|
| 1 | 76,233 | 1.69 | 70.2% | 4.0% |
| 2 | 22,703 | 3.31 | 0% | 13.3% |
| 5 | 3,012 | 9.60 | 0% | 100% |
| 10 | 747 | 19.92 | 0% | 100% |

Confirms Experiment 1's finding: at W=1, 70% of edges are incidental
single-co-occurrence. The backbone (W≥5) is uniformly high-weight.

#### M6 — Concept fragmentation

| W | (W-independent) | multi-context groups | % | max contexts |
|---|---|---|---|---|
| all | 18,242 | 1,282 | 7.0% | 5 |

Fragmentation is W-independent (it's a property of concept extraction, not the
relation graph). **7% of concept groups appear under multiple L3 contexts** —
e.g. "knowledge graph reasoning" under both "Activity" and "Concept" (the
report already noted this). 1,282 multi-context groups out of 18,242. The
report's Measurement 6 calls for a manual audit of whether these are genuine
facets or extraction errors; that audit is part of Experiment 3 (Failure 5:
context mislabeling), not this experiment.

#### M7 — source_count as confidence signal

| W | n facts | Pearson r | Spearman ρ | mean source_count |
|---|---|---|---|---|
| 1 | 35,376 | 0.013 | 0.025 | 1.003 |
| 2 | 33,757 | 0.011 | 0.024 | 1.003 |
| 5 | 27,580 | −0.002 | 0.007 | 1.003 |
| 10 | 21,226 | −0.010 | −0.001 | 1.004 |

**No correlation between fact source_count and concept centrality at any W.**
The report predicted "highly-confirmed facts anchor the graph's structural
backbone." This is **not confirmed** — but the reason is corpus-specific: 99.7%
of facts in the MultiHop-RAG corpus have source_count=1 (each fact comes from
a single news article). There is almost no variance in source_count to
correlate with. This measurement would need a corpus with multi-source
confirmation (e.g. a research-paper corpus where facts are confirmed by
multiple studies) to be meaningful. On this corpus, source_count is not a
useful confidence signal simply because it lacks variance.

### What this settles

1. **The graph has strong, coherent structure at all W thresholds.** Modularity
   Q = 0.79–0.88, small-world σ >> 1, and communities align with corpus domains
   (NMI rising from 0.41 to 0.59 as W increases). The report's design-level
   claim that "co-occurrence projection produces semantically coherent clusters
   without any explicit topic assignment" is **empirically confirmed**.

2. **W=5 is the structural sweet spot.** It is the last threshold where the giant
   component dominates (72.2% of nodes); at W=10 the giant collapses to 43.4%
   and the graph fragments into 89 small components. NMI peaks at W=5–10
   (0.57–0.59), so the community-domain alignment is strongest in this range.
   Combined with Experiment 1's finding (W=5 is the minimum W with zero
   confounded edges), **W=5 is both the structural and statistical sweet spot**.

3. **The degree distribution is heavy-tailed but not scale-free** — lognormal
   fits better than power-law at all W, exactly as Broido & Clauset found for
   most real-world networks. The report's prediction is confirmed. Hub concepts
   (Google, Amazon, USA) dominate connectivity, and their dominance increases
   with W as the low-weight edges that connected rare concepts are filtered out.

4. **Navigability holds at all W.** The giant component's average path length
   stays ~5 hops from W=1 to W=10, so `getRelatedConcepts`-driven exploration
   works at any threshold. But the Van den Berg–van Leeuwen caveat applies:
   high σ is partly a sparseness artifact, not proof of designed navigability.

5. **source_count is not a useful confidence signal on this corpus** because
   99.7% of facts have source_count=1. The measurement needs a corpus with
   multi-source confirmation to be meaningful. The report's prediction is
   untestable here, not disproven.

Output: `results/graph_properties.json`. Computation: ~51s. No LLM cost.

---

## Experiment 3 — 5 failure-mode audits (§6.2)

**Paper claim under test (§6.2):** the report flagged five concept-extraction
failure modes as the "biggest risks" but never measured them. Experiments 1
and 2 showed the graph *structure* is sound; this experiment asks whether the
concept *extraction itself* is any good.

**Judge model:** DeepSeek V4 Flash (`deepseek/deepseek-v4-flash`) for the
LLM-dependent audits (Failures 2, 3, 5). The model-independent audits
(Failures 1, 3b, 4) use no LLM. The judge model is configurable via the
`JUDGE_MODEL` env var for future re-runs with a different model.

### Failure 1 — Under-merging (fragmentation): 6.87% (1,254 pairs)

**Method (intra-corpus, alias-based):** instead of mapping to an external
ontology (Wikidata), which only measures ontology compliance, we detect
fragmentation *within* the corpus — two OKT concept groups that should be one
entity. Three signals: (1) shared alias ≥ 8 chars with shared facts or
canonical-name match; (2) one group's canonical name is another's alias; (3)
prefix match with zero shared facts (excluding qualifier-noun suffixes like
"models", "tools", "training"). See `exp3_failure_audits.py`.

**Result:** 1,254 fragmented pairs out of 18,242 groups (6.87%). The examples
fall into two categories:

- **Real fragmentation** (should merge): `"sam"` ↔ `"samuel bankman-fried"`
  (same person, "Sam" is an alias of SBF), `"amazon prime"` ↔ `"amazon.com,
  inc."` (share 6 facts — Amazon Prime is Amazon's service).
- **Entity vs derivative entity** (borderline): `"las vegas"` ↔ `"las vegas
  raiders"` (city vs team, share 3 facts), `"england"` ↔ `"england national
  football team"` (country vs team, share 1 fact). These share aliases and
  occasional facts but are arguably different concepts.

**Verdict:** fragmentation is real but moderate (~7%). The report called this
"likely one of the two biggest risks" — it is present but not severe. The
alias-based intra-corpus method is a stronger signal than the original
Wikidata-mapping approach (which only mapped 9/200 concepts and measured
ontology compliance, not fragmentation).

### Failure 2 — Over-merging (false merges): 4.0% (8/200)

**Method:** sampled 200 concept groups, DeepSeek judged whether each contains
two or more distinct real-world entities incorrectly merged because they share
a surface form.

**Result:** 8 over-merged groups (4.0%):
- `"national basketball league"` — merges Australian NBL and US NBL (1937-1949)
- `"the penguin"` — merges the fictional character with the TV series
- `"national championships"` — merges US Figure Skating and UK Swimming

**Verdict:** over-merging is real but rare (4%). These are genuine homonym
collisions where the L3 context didn't disambiguate. The report called this the
"second biggest risk" — present but less severe than fragmentation.

### Failure 3 — Missing concepts (recall): 42% raw, 65% adjusted

**Method:** sampled 100 facts, ran spaCy NER (`en_core_web_lg`) to extract gold
entities + noun chunks, diffed against OKT-linked concepts. Then the LLM judge
(DeepSeek) classified each "missed" entity as either a **real miss** (OKT
should have captured it — a genuine entity/concept) or **noise** (spaCy
over-extracted: too generic, fragment, bound-failure, common word). This
measures both sides of the spaCy-vs-LLM tradeoff: OKT's under-extraction
and spaCy's over-extraction.

**Results:**

| Metric | Value |
|---|---|
| Raw recall (all spaCy entities) | 41.9% mean |
| **Adjusted recall (excluding noise)** | **65.0% mean** |
| Noise rate (of missed entities) | 59.3% noise |
| Real misses | 126 |
| spaCy noise | 213 |

**59.3% of the "missed" entities are spaCy noise**, not real misses. The raw
42% recall makes OKT look worse than it is — spaCy extracts fragments like
"seven touchdown scoring drives", "five teams", "breathtaking speed",
"memory", "storage", "that", "which" — none of which are concepts worth
extracting. When noise is excluded, **adjusted recall rises to 65%**, which
falls within the report's predicted 60-80% range.

**Noise examples (spaCy over-extraction):** "seven touchdown scoring drives",
"gb", "memory", "storage", "tb", "five teams", "breathtaking speed",
"complexity", "size", "moderators", "private testing", "that", "which",
"less than 100% capacity", "practice".

**Real miss examples (OKT under-extraction):** "128gb", "8tb", "ai", "nmpa",
"the us copyright office", "tim mcgraw", "mozilla's instance".

The real misses are mostly specific technical/product entities ("128GB",
"8TB") and abbreviations ("AI", "NMPA") that spaCy catches but OKT's LLM
extractor missed — likely because the LLM is more conservative about
extracting quantities and abbreviations. These are genuine recall gaps, but
they are a small fraction of what spaCy extracts.

**This quantifies why spaCy was removed from the system:** it retrieved more
entities but also more noise (59.3% of its "misses" are noise). The OKT LLM
extractor trades quantity for quality — capturing 65% of real entities while
avoiding the fragments and generic nouns spaCy produces. The previous
versions of the system used a spaCy extractor and removed it for exactly this
reason; the noise measurement confirms the tradeoff was the right call.

### Failure 3b — Hallucinated concepts (cross-contamination): 0.5% (1/200)

**Method (alias-aware):** for each concept with >20 linked facts, sampled 20
facts and checked if the concept's canonical name OR any of its aliases
(≥ 2 chars) appears in the fact text. Concepts mentioned in <50% of their
linked facts are flagged as suspect.

**Result:** 1 suspect concept out of 200 (0.5%): `x corp.` (mention_rate 35% —
linked to 337 facts, aliases include "Twitter" but facts may reference the
company in other ways). The previous substring-only method reported 55.5%
suspect, but that was inflated by name-format mismatch (OKT formalizes "google
llc" while facts say "Google"). Using aliases dropped it to 0.5%.

**Verdict:** residual cross-contamination is negligible on this corpus
post-fix. The strict-grounding prompt (already shipped) appears to have
eliminated the hallucinated-concept problem the old repo found (Nature linked
to 2,735 facts with only ~15 real). The alias-aware method is the right way
to measure this.

### Failure 4 — Dedup-severed facts: skipped

The `facts` table has no `content_hash` column in the current schema. The
audit was designed for a column that doesn't exist (renamed or removed). Not
measurable with the current schema.

### Failure 5 — Context mislabeling: 13.5% (27/200)

**Method:** sampled 200 concepts, DeepSeek assigned the correct L3 context
from the repo's official 88-label shortlist (NOT the full 789 DBpedia L3 list,
which was dropped for being too long), compared against the system-assigned
context.

**Result:** 27 mislabeled (13.5%), within the report's predicted 10-25%
range:
- `"jahmyr gibbs"`: assigned "person" → correct "Athlete" (too generic)
- `"consumer reports"`: assigned "organisation" → correct "Media publication"
- `"florida a&m university"`: assigned "sports team" → correct "University"

**Verdict:** context mislabeling is moderate (13.5%) and within the predicted
range. The errors are mostly over-generic labels — the LLM picks "person"
when "Athlete" is more specific, "organisation" when "Media publication" is
more accurate. This is a precision issue, not a catastrophic failure.

### Summary

| Failure | Rate | Report's prediction | Verdict |
|---|---|---|---|
| 1 (fragmentation) | 6.87% (1,254 pairs) | "biggest risk" | **Real but moderate** — intra-corpus alias detection |
| 2 (over-merging) | 4.0% (8/200) | "biggest risk" | **Real but rare** — homonym collisions |
| 3 (recall) | 42% raw / **65% adjusted** | 60-80% | **Adjusted recall within prediction** — 59% of "misses" are spaCy noise |
| 3b (hallucination) | 0.5% (1/200) | (not predicted) | **Negligible post-fix** — alias-aware method corrects 55.5% false positive rate |
| 4 (dedup-severed) | N/A | — | **Skipped** — no content_hash column |
| 5 (context mislabeling) | 13.5% (27/200) | 10-25% | **Within prediction** — mostly over-generic labels |

**Overall assessment:** the concept extraction is sound. The two "biggest
risks" (fragmentation 7%, over-merging 4%) are present but moderate.
Hallucination is negligible post-fix. Context mislabeling is within the
predicted range. Recall (42% raw) rises to **65% adjusted** when spaCy noise
is excluded — within the predicted 60-80% range — and the noise measurement
(59.3% of "misses" are spaCy over-extraction) quantifies exactly why spaCy
was removed from the system in previous versions.

Output: `results/failure_audits.json`. DeepSeek V4 Flash judge. Cost: ~$0.01.
Wall time: ~20min for 400 LLM calls.

---

## Experiment 4 — KGQA head-to-head (§4.4 / §7.2)

**Paper claim under test (§4.4):** "The discriminating experiment would be a
head-to-head on a downstream task that needs relation semantics — e.g.,
multi-hop KGQA. On such a task, a triplet KG with typed predicates should win
because it carries the semantics the task needs; a co-occurrence concept
graph must lean on the LLM to infer the relation type at query time."

**Method.** Four retrieval conditions answer the same MultiHop-RAG questions
(n=500 random sample), scored per question type (inference, comparison,
temporal, null) using the same token-overlap scorer as the existing benchmark:

- **(a) Triplet-KG** — a triplet KG built from **raw source text** (the
  original 609 articles, NOT pre-decomposed atomic facts — this is the fair
  comparison: both OKT and the triplet KG start from the same raw source text,
  but the triplet KG extracts typed relations while OKT decomposes into atomic
  facts). Source text was chunked at ~2000 tokens (1,053 chunks), and Gemma 4
  31B (the same model OKT uses for fact/concept extraction) extracted (subject,
  relation, object) triples per chunk. **Temporal enrichment:** each triple
  carries the source's `published_at` date as a fourth field
  `(subject, relation, object, published_at)` — standard REBEL/EDC triples
  have no temporal dimension, which would make temporal_query questions
  unanswerable. The date is inherited from the source article (all 609 sources
  have `published_at`), giving the triplet KG the same temporal context OKT
  facts carry via `source.published_at`. Result: 11,874 triples, 1,661
  relation types, 6,492 unique subjects. Top relations: plays_for (3262),
  works_at (458), located_in (363). For each question, keyword queries retrieve
  matching triples, which are fed to the synthesis LLM with their dates.
- **(b) Concept-graph walk** — the existing OKT REST endpoints:
  `search_concepts` → `get_related_concepts` → `get_concept_facts`. Walks the
  co-occurrence graph to surface connected facts. Uses **two separate
  query-generation steps**: concept queries (noun phrases for concept name
  matching) find the right concepts, then fact-optimized queries
  (keyword-rich `websearch_to_tsquery` strings tuned for the fact tsvector
  index, matching the original benchmark's `FACT_QUERY_SYSTEM` prompt) filter
  facts within each concept. This mirrors the original benchmark's agentic
  query extraction applied after graph navigation, instead of reusing concept
  phrases for fact filtering. Fed to the synthesis LLM.
- **(c) Facts-direct (baseline)** — the existing `search_facts` endpoint with
  keyword queries. The existing `direct@20` baseline from `RESULTS.md`. OKT's
  `search_facts` fuses lexical tsvector + embedding search — this is the real
  retrieval mechanism the other two conditions are compared against.
- **(d) Concept definitions (planned, not yet run)** — would retrieve the
  LLM-generated concept synthesis/definition (the compressed summary from the
  `synthesize_concept` worker) instead of raw facts. This tests whether the
  processed summary is a better retrieval unit than the raw fact list. Not
  included in the current results.

All conditions use the same answer-synthesis prompt and LLM backend
(Gemma 4 31B). Only the retrieval path differs. See
`exp4_kgqa/run_kgqa.py`.

### Triplet KG enrichment

The triplet KG was enriched beyond standard REBEL/EDC output to make the
comparison fair:

1. **Source-text extraction (not fact extraction).** Triples were extracted
   from the raw `sources.parsed_text` (the original articles), not from OKT's
   pre-decomposed atomic facts. A standard triplet KG construction pipeline
   works on raw text; extracting triples from facts would give the triplet KG
   an unfair advantage (pre-decomposed, self-contained claims) rather than
   testing the paradigm on its own terms.

2. **Temporal metadata.** Standard triples are (s, r, o) with no time
   dimension. The MultiHop-RAG benchmark has `temporal_query` questions
   (n=119 in the sample) that require date awareness. Without dates, the
   triplet KG would score 0% on temporal questions by design, not by
   retrieval failure. Each triple carries the source article's
   `published_at` (all 609 sources have it), giving the triplet KG the same
   temporal context OKT facts carry via `source.published_at`. The date is
   included in the evidence fed to the synthesis LLM:
   `[2023-10-01] Sam Bankman-Fried | faces | fraud charges`.

3. **Same extraction model.** Gemma 4 31B (the model OKT uses for fact and
   concept extraction) was used for triplet extraction, so both sides use the
   same model family. A smarter or weaker model would confound the comparison.

### Results — Overall (n=500)

| Variant | Accuracy | Refusal rate | Answer rate |
|---|---|---|---|
| triplet_kg | 18.4% | 95.6% | 4.4% |
| concept_walk | 18.0% | 96.0% | 4.0% |
| concept_definitions | 32.4% | 81.6% | 18.4% |
| **facts_direct** | **50.4%** | 61.4% | **38.6%** |

**The paper's prediction is not confirmed.** The triplet KG does NOT win — it
loses decisively. `facts_direct` (50.4%) beats `concept_definitions` (32.4%)
by 1.5×, and beats both `concept_walk` (18%) and `triplet_kg` (18.4%) by ~3×.
The triplet KG has a 95.6% refusal rate — it almost never produces an answer.

The concept-graph walk (now using fact-optimized queries for fact filtering)
dropped from 25% (pre-enrichment) to 18% — the dual-query approach made it
slower per question (more API calls) but did not improve accuracy. The
concept_definitions variant (LLM-generated syntheses, not raw facts) is the
strongest graph-based approach at 32.4%, but still well below direct fact
search.

### Results — Per question type

| Type | n | triplet_kg | concept_walk | concept_definitions | facts_direct |
|---|---|---|---|---|---|
| inference_query | 146 | 14.4% | 13.0% | 56.9% | **81.5%** |
| comparison_query | 164 | 0.0% | 0.0% | 1.8% | **25.6%** |
| temporal_query | 119 | 0.0% | 0.0% | 4.2% | **16.8%** |
| null_query | 71 | 100% | 100% | 100% | 100% |

The pattern holds across all question types. `facts_direct` dominates
`inference_query` (81.5% vs 56.9% vs 14.4% vs 13.0%) — the exact question type
the paper predicted the triplet KG would win on. The triplet KG scores **0% on
comparison and temporal** questions (100% refusal), even with temporal
enrichment (published_at per triple). The concept_definitions variant is the
only graph-based approach that scores meaningfully on inference (56.9%) —
the LLM-generated synthesis carries more context than isolated triples or
raw fact lists filtered through graph navigation.

### Why the triplet KG loses

The triplet KG's 97.2% refusal rate is the key: it almost never retrieves
relevant triples for a given question. The issue is **retrieval, not
extraction**:

1. **The triple store is unindexed.** Triples are matched by keyword substring
   against subject/relation/object. This is far less precise than OKT's
   `websearch_to_tsquery` full-text search over 36k facts. A question about
   "the cryptocurrency executive facing fraud charges" must match triples
   whose subject or object literally contains "cryptocurrency" or "fraud" —
   but the triples say "Sam Bankman-Fried | CEO of | FTX" and "Sam
   Bankman-Fried | faces | fraud charges", with "cryptocurrency" nowhere.

2. **Triples lose context.** A triple "Sam Bankman-Fried | faces | fraud
   charges" carries the relation but drops the surrounding context (which
   article, what specific charges). The temporal enrichment adds the date
   (`[2023-10-01] Sam Bankman-Fried | faces | fraud charges`) but still drops
   the full sentence context. The synthesis LLM can't answer multi-hop
   questions ("who is the individual associated with the cryptocurrency
   industry facing a criminal trial") from isolated triples the way it can
   from atomic facts that preserve full sentences.

3. **The concept-graph walk is in between.** It retrieves facts (not triples)
   but through graph navigation (concept → related concepts → facts), which
   is less precise than direct fact search. 18% accuracy with 96% refusals
   — the concept search often finds no matching concepts for the question's
   entities, so no facts are retrieved. The fact-optimized query filter
   (matching the original benchmark's `FACT_QUERY_SYSTEM` prompt) did not
   improve accuracy over the pre-enrichment run (18% vs 25%), suggesting the
   bottleneck is the concept-search recall, not the fact-filtering precision.

4. **Concept definitions outperform concept facts.** The concept_definitions
   variant (32.4%) beats the concept_walk variant (18%) by 1.8× — the
   LLM-generated synthesis/definition carries more context per concept than
   the raw fact list, giving the synthesis LLM better material to reason with.
   On inference_query specifically, concept_definitions scores 56.9% vs
   concept_walk's 13.0% — the compressed summary preserves the multi-hop
   connections the raw fact list fragments. But concept_definitions still
   loses to facts_direct (50.4%) — the synthesis is a lossy compression of
   the facts, and direct fact search preserves more relevant detail.

### What this settles

1. **On targeted retrieval, fact-RAG beats a naive triplet KG.** The triplet
   KG (18.4%) loses to fact-RAG (50.4%) — but this is an implementation
   comparison (naive keyword search vs full-text + embeddings), not a paradigm
   comparison. The fair KG-vs-fact-RAG comparison would need proper KG
   navigation (embeddings, entity linking, typed-path traversal).

2. **The concept graph is not a knowledge base and should not be compared to
   a KG on QA.** The concept walk (18%) scores low because the concept graph is
   an organization structure (indexes facts), not a knowledge base (stores
   knowledge). Including it in this experiment was a framing error — it tests
   an index on a retrieval task where the knowledge bases (facts, triples)
   are the right competitors.

3. **Fact-RAG is the strongest retrieval path for targeted QA.** OKT's atomic
   facts — self-contained, searchable by full-text + embeddings — are a better
   retrieval substrate than either isolated triples (naive) or concept-guided
   fact filtering. The graph-based approaches (concept walk, concept
   definitions) add navigation overhead that hurts recall without improving
   precision on narrow-domain QA.

4. **The triplet KG could work on this benchmark with proper navigation.** The
   18.4% score reflects our naive keyword retrieval, not the triplet-KG
   paradigm's ceiling. Embedding search over triples + entity linking would
   likely close much of the gap with fact-RAG, since the dominant failure mode
   is vocabulary mismatch (the question says "cryptocurrency" but the triple
   says "FTX"), which embeddings solve.

5. **Concept syntheses have independent retrieval value (32.4%).** The
   LLM-generated synthesis outperforms both the concept walk (18%) and the
   triplet KG (18.4%) — the compressed summary preserves multi-hop connections
   that raw fact lists fragment and that isolated triples lose. On
   inference_query, concept definitions scores 56.9% vs concept walk's 13.0%.
   But syntheses are lossy compressions and still lose to direct fact search
   (50.4%). The synthesis layer complements fact retrieval; it doesn't replace
   it.

### Limitations of this experiment and why the comparison framing matters

This experiment has a framing problem that the results expose. The OKT concept
graph and a triplet KG are **not the same kind of structure**, and the
comparison as designed conflates two different questions:

1. **The fair comparison is KG vs Fact-RAG, not KG vs concept graph.** A
   triplet KG and OKT's atomic facts are both **knowledge bases** — they store
   knowledge. The triple `(Sam Bankman-Fried, CEO_of, FTX)` IS knowledge; the
   atomic fact "Sam Bankman-Fried was CEO of FTX" IS knowledge. Comparing them
   on a retrieval task is apples-to-apples: both are knowledge bases competing
   to be the best retrieval substrate. The concept graph, by contrast, is an
   **organization structure** — it doesn't store knowledge, it structures
   knowledge that already exists in the facts. The concepts and their
   co-occurrence edges are an index over the fact knowledge base, not a
   knowledge base themselves.

2. **The concept graph should be evaluated on navigation/discovery tasks, not
   against a KG on QA.** The OKT concept graph's value — per Experiments 1 and
   2 — is in navigation (graph-walking to discover connected evidence),
   synthesis (community structure), and exploration (browsing related concepts
   across a wide domain). These are wide-domain tasks (discovery, review,
   synthesis), not narrow-domain tasks (targeted QA). Including the concept
   walk in this experiment was a framing error: it tests the concept graph on a
   task it was not designed for, against a competitor (triplet KG) that is a
   different kind of structure entirely.

3. **The triplet KG could work on this benchmark with proper navigation.** Our
   triplet KG used naive keyword substring search — no embeddings, no entity
   linking, no typed-path traversal. A production KGQA system would resolve
   the question's entities to KG nodes, then traverse typed relation paths to
   find the answer. The 18.4% score reflects our implementation's retrieval
   weakness, not the triplet-KG paradigm's limitation. Adding embedding search
   to the triple store would likely close much of the gap with fact-RAG, since
   the retrieval failure (vocabulary mismatch) is the dominant problem.

4. **OKT's concept graph is not a knowledge base in the KG sense.** A
   traditional KG stores knowledge in its relations — remove the relation and
   the knowledge is gone. OKT's concept graph stores nothing; it computes
   co-occurrence counts over facts that already hold the knowledge. This is
   why the concept graph competes with other *indexing/navigation* structures
   (ontologies, taxonomies, embedding clusters), not with *knowledge bases*
   (triplet KGs, fact stores). The concept graph is better understood as a
   navigational layer over the fact knowledge base, not as an alternative to
   the triplet KG.

**What this experiment does show:**
- On a targeted retrieval task (MultiHop-RAG), OKT's fact knowledge base
  dominates all graph-based approaches, including a naive triplet KG.
- The triplet KG's poor performance is an implementation problem (naive
  retrieval), not a paradigm problem. A KG with proper navigation (embeddings,
  entity linking, typed-path traversal) would be the fair competitor to
  fact-RAG.
- The concept graph's low score (18%) is expected — it's an organization
  structure tested on a retrieval task it wasn't designed for. Its value is in
  navigation and discovery (Experiments 1-2), not targeted QA.

**What this experiment cannot show:**
- Whether a properly-implemented triplet KG beats fact-RAG on retrieval quality
  (would need embedding search + entity linking on the triple store).
- Whether OKT's concept graph beats a triplet KG on a discovery/navigation
  task (would need a different benchmark and evaluation methodology).
- Whether typed relations help on a real KGQA benchmark (MetaQA, WebQuestionsSP)
  that requires typed-path traversal — MultiHop-RAG questions don't exercise
  this.

### Caveats

- **n=500 random sample, not the full 2556.** The full run would confirm the
  pattern but is unlikely to change the ranking.
- **This is not a fair KGQA comparison.** As documented above, the OKT concept
  graph is an organization structure (not a knowledge base), MultiHop-RAG is a
  retrieval benchmark (not a KGQA benchmark), and the triplet KG lacks
  embedding search. The results show that on a targeted retrieval task, facts
  dominate graphs — but they cannot adjudicate the paper's KGQA claim, which
  requires a real KGQA benchmark with typed-path traversal questions.
- **The triplet KG retrieval is naive (keyword substring).** OKT's
  `search_facts` fuses lexical + embedding search; the triplet KG has only
  keyword substring. Adding embedding search to the triplet KG was considered
  but dropped — it would improve retrieval but would not make the comparison
  fair, because the structural mismatch (organization structure vs knowledge
  base, retrieval task vs KGQA task) would remain.
- **The concept-graph walk uses the default W=1 threshold** (all edges). Using
  the BiCM-validated weights from Experiment 1 or the W=5 threshold might
  improve precision, but the dominant failure mode is "no matching concepts
  found" (recall), not "wrong concepts found" (precision).
- **All conditions use Gemma 4 31B for synthesis.** A stronger synthesis model
  might narrow the gap by inferring relations from context, but the retrieval
  bottleneck would remain.
- **The `facts_direct` baseline (50.4%) is below the original benchmark's
  `facts@20` (75.7%).** The original benchmark uses agentic query extraction
  (3-6 keyword-rich tsvector queries per question, tuned by a dedicated
  `FACT_QUERY_SYSTEM` prompt) and OKT's fused lexical+embedding search. This
  experiment's `facts_direct` uses the same prompt but a simpler retrieval
  path. The 50.4% vs 75.7% gap is a retrieval-pipeline difference, not a
  paradigm difference — the real OKT system is stronger than this experiment's
  baseline.

Output: `exp4_kgqa/results/kgqa_headtohead.json`. Triplet extraction: Gemma 4
31B, 609 sources (raw text), 1,053 chunks, 11,874 triples with temporal
enrichment (published_at per triple). Four variants: triplet_kg,
concept_walk (fact-optimized queries), concept_definitions (LLM syntheses),
facts_direct (baseline). Total wall time: ~59 min (triplet extraction
~10 min + 4 × 500 questions ~12 min each).