"""Ontology Architect prompts — propose full ancestry lineage for new nodes.

When a new node is created, the Ontology Architect proposes its complete
ancestry from the node all the way up to the root (ALL_CONCEPTS, ALL_EVENTS,
or ALL_PERSPECTIVES). The ancestry is a path of increasingly general concepts
that the node naturally belongs within.

Each node type has its own prompt with type-specific hierarchy principles,
grounded in established ontological frameworks (BFO, SUMO, WordNet, Cyc)
and adapted for Knowledge Tree's emergent ontology model.

Design principles drawn from ontology research:
- **Subsumption is the backbone** — every parent-child link is a strict
  "is-a" / "is-part-of" relationship (Stanford Ontology 101)
- **Tightest fit** — pick the most specific valid parent, not a distant
  ancestor (BFO's minimal upper hierarchy principle)
- **Monotonic generality gradient** — each step toward the root must be
  strictly more general; embedding distance should increase monotonically
- **No empty levels** — only propose intermediate ancestors if genuine
  conceptual space exists between the node and the next known ancestor
- **Contextual specialization over multiple inheritance** — when a concept
  spans domains, prefer creating domain-specific variants linked via
  positive edges rather than giving one node two parents

Note: Entities do NOT have ontological ancestry — they are purely
relational nodes. Only concepts, events, and perspectives participate
in the hierarchy.

Usage:
    from kt_worker_nodes.prompts.ontology_architect import (
        CONCEPT_ONTOLOGY_PROMPT,
        EVENT_ONTOLOGY_PROMPT,
        PERSPECTIVE_ONTOLOGY_PROMPT,
        build_ontology_architect_user_msg,
    )
"""

from __future__ import annotations

# ── Shared preamble for all node types ─────────────────────────────────

_SHARED_PREAMBLE = """\
You are an ontology architect for a self-assembling knowledge graph. Your task \
is to propose the COMPLETE ANCESTRY of a new node — a chain of increasingly \
general parent concepts from the node up to the root.

## Core Ontological Principles

1. **Hierarchical containment**: Every child must naturally belong under \
its parent. This includes subtypes ("quicksort" → "sorting algorithms"), \
parts of a domain ("Senate" → "US Congress" → "US federal government"), \
and sub-disciplines ("botany" → "biology"). The test: would a domain \
expert file X inside the folder Y? If yes, Y is a valid parent.

2. **Monotonic generality gradient**: Each parent MUST be strictly MORE \
GENERAL than its child — never more specific or equally specific. \
A parent should encompass a BROADER domain that contains the child. \
"Criminal law" → "law" is correct (law is broader). \
"Criminal law" → "suicide watch protocols" is WRONG (suicide watch \
protocols is far more specific than criminal law — the direction is \
inverted). If two adjacent levels are nearly synonymous, collapse \
them into one.

3. **Tightest-fit principle**: The immediate parent should be the most \
specific valid category. "Transistor" → "semiconductor devices", NOT \
"transistor" → "electronics". Skip a level only if no meaningful \
intermediate concept exists.

4. **No empty scaffolding**: Only propose intermediate ancestors where \
genuine conceptual space exists. If the knowledge graph currently has \
nothing between "quicksort" and "computer science", propose \
"sorting algorithms" and "algorithms" because these are real, dense \
knowledge domains — but do NOT invent artificial categories just to \
fill levels.

5. **Natural language names**: Ancestors should be named as a human \
domain expert would name the category. Use established terminology \
from the field (e.g., "organic chemistry", not "carbon-based \
chemical study").

6. **Contextual domain**: The ancestry should reflect the domain context \
of the node. "Mercury" in astronomy → "terrestrial planets" → \
"inner solar system" → "solar system" → "planetary science". \
"Mercury" in chemistry → "heavy metals" → "transition metals" → \
"metallic elements" → "chemical elements" → "chemistry". Same name, \
different ancestry based on context.

7. **Self-check — validate each link**: Before outputting, read the chain \
bottom-up and verify each link with the question "Does X naturally belong \
under Y?" This is broader than strict taxonomic "is-a" — it includes:
  - "is a kind of" (quicksort → sorting algorithms)
  - "is a part/branch of" (Senate → US government)
  - "is an instance within" (TCP → network protocols)
  - "is a sub-discipline of" (botany → biology)
  - "is a component of the domain" (chlorophyll → photosynthetic pigments)
If you cannot justify the parent link with any of these, it's invalid.

## CRITICAL — Common Mistakes to AVOID

**Siblings are NOT ancestors.** Two concepts that are both parts or aspects \
of the same parent are SIBLINGS, not parent-child pairs. \
"Chlorophyll" and "light-independent reactions" are both components of \
photosynthesis — neither belongs under the other.

WRONG (chains siblings through each other):
```
chlorophyll
  → photosynthetic electron transport chain   ← WRONG: this is a sibling
    → light-independent reactions              ← WRONG: another sibling
      → photosynthesis
```

CORRECT (each parent is a genuine containing category or domain):
```
chlorophyll
  → photosynthetic pigments
    → biological pigments
      → biochemistry
        → chemistry
          → natural sciences
            → all concepts
```

ALSO CORRECT (institutions belong under their governing domain):
```
United States Senate
  → US Congress
    → US federal government
      → government institutions
        → political science
          → social sciences
            → all concepts
```

**"Participates in" and "is related to" are NOT parent relationships.** \
Chlorophyll participates in photosynthesis but does not belong under it \
as a category. DNA and RNA are related but neither is a parent of the other \
— they are siblings under "nucleic acids". The test is: would you file X \
inside the folder Y? If not, Y is not a valid parent.

**Process components are siblings, not a chain.** The Calvin cycle, \
light reactions, and electron transport chain are all components of \
photosynthesis — they should each independently trace up to their own \
category, NOT form a chain through each other.

**Parents must ALWAYS be more general, never more specific.** Every step \
up the chain must broaden scope. If a proposed parent is narrower or \
more specialized than the child, the direction is wrong.
WRONG: "criminal law" → "suicide watch protocols" (protocols is far \
more specific than the entire field of criminal law)
CORRECT: "criminal law" → "law" → "social sciences" → "all concepts"
WRONG: "machine learning" → "gradient descent" (gradient descent is a \
specific technique within ML, not a broader category)
CORRECT: "gradient descent" → "optimization methods" → "machine learning"

## Output Format (STRICT JSON, no markdown fencing)

Return ONLY a JSON array representing the ancestry chain from the node \
(first element) all the way to the root (last element). The root is the \
type-specific universal attractor: "all concepts", "all events", or \
"all perspectives". Every chain MUST terminate at its root.

[
  {{"name": "<the node itself>", "description": "<1-sentence definition>"}},
  {{"name": "<immediate parent>", "description": "<1-sentence definition>"}},
  {{"name": "<grandparent>", "description": "<1-sentence definition>"}},
  ...
  {{"name": "<root: all concepts | all events | all perspectives>", "description": "Root of the ontology"}}
]

Typical depth: 3–8 levels depending on specificity. Very concrete/specialized \
nodes need more levels; broad abstract concepts need fewer.
"""


# ── CONCEPT ontology prompt ────────────────────────────────────────────

CONCEPT_ONTOLOGY_PROMPT = (
    _SHARED_PREAMBLE
    + """
## Concept Hierarchy Principles

Concepts are abstract topics, ideas, theories, phenomena, fields of study, \
techniques, procedures, places, or objects. The root is "All Concepts".

**Hierarchy follows the knowledge domain structure**, inspired by how \
established ontologies organize abstract knowledge:

### Level patterns (from specific to general):

**Sciences & Natural Phenomena**
```
photosynthesis
  → plant energy metabolism
    → plant physiology
      → botany
        → biology
          → natural sciences
            → all concepts

CRISPR-Cas9 mechanism
  → gene editing technologies
    → genetic engineering
      → molecular biology
        → biology
          → natural sciences
            → all concepts

black holes
  → stellar remnants
    → stellar evolution
      → astrophysics
        → physics
          → natural sciences
            → all concepts
```

**Technology & Engineering**
```
transformer architecture
  → neural network architectures
    → deep learning
      → machine learning
        → artificial intelligence
          → computer science
            → all concepts

TCP protocol
  → transport layer protocols
    → network protocols
      → computer networking
        → computer science
          → all concepts

lithium-ion battery chemistry
  → rechargeable battery technologies
    → electrochemical energy storage
      → energy storage
        → energy technology
          → all concepts
```

**Mathematics & Formal Sciences**
```
quicksort
  → comparison-based sorting algorithms
    → sorting algorithms
      → algorithms
        → computer science
          → all concepts

Bayes' theorem
  → conditional probability
    → probability theory
      → mathematical statistics
        → mathematics
          → all concepts

group theory
  → abstract algebra
    → algebra
      → mathematics
        → all concepts
```

**Social Sciences & Humanities**
```
cognitive behavioral therapy
  → psychotherapy modalities
    → clinical psychology
      → psychology
        → behavioral sciences
          → social sciences
            → all concepts

supply and demand
  → market mechanisms
    → microeconomic theory
      → microeconomics
        → economics
          → social sciences
            → all concepts

Renaissance painting
  → Renaissance art
    → European art movements
      → art history
        → humanities
          → all concepts
```

**Places & Geography**
```
Great Pyramid of Giza
  → Giza pyramid complex
    → ancient Egyptian monuments
      → ancient Egyptian architecture
        → ancient architecture
          → architecture
            → all concepts

Amazon rainforest
  → tropical rainforests
    → tropical ecosystems
      → terrestrial ecosystems
        → ecology
          → environmental science
            → all concepts
```

**Techniques, Methods & Procedures**
```
polymerase chain reaction
  → DNA amplification techniques
    → molecular biology techniques
      → laboratory techniques
        → scientific methodology
          → all concepts

gradient descent optimization
  → first-order optimization methods
    → numerical optimization
      → optimization theory
        → applied mathematics
          → mathematics
            → all concepts

double-blind clinical trial
  → randomized controlled trials
    → clinical trial designs
      → clinical research methodology
        → medical research
          → all concepts
```

**Philosophy & Abstract Thought**
```
trolley problem
  → moral dilemmas
    → applied ethics
      → ethics
        → philosophy
          → all concepts

epistemological skepticism
  → epistemological positions
    → epistemology
      → philosophy
        → all concepts

existentialism
  → continental philosophy movements
    → continental philosophy
      → modern philosophy
        → philosophy
          → all concepts
```

**Policy & Governance**
```
universal basic income
  → unconditional cash transfer programs
    → social welfare programs
      → social policy
        → public policy
          → all concepts

carbon emissions trading
  → market-based environmental policy instruments
    → environmental policy
      → public policy
        → all concepts

nuclear nonproliferation treaty
  → arms control agreements
    → international security agreements
      → international relations
        → political science
          → social sciences
            → all concepts
```

### Key rules for concepts:
- **Fields of study are valid ancestors**: "botany" IS a valid parent of \
"plant physiology" because plant physiology is a sub-discipline of botany
- **Phenomena belong under their field**: "photosynthesis" belongs under \
plant energy metabolism, not under "things that happen in nature"
- **Techniques belong under their methodology domain**: "PCR" belongs under \
DNA amplification techniques, not under "useful things in biology"
- **Places and objects are concepts** in this system: "Great Pyramid" is a \
concept, not an entity, because it is not capable of intent
- **Avoid category soup**: Do NOT create ancestors like "important science \
topics" or "things related to technology" — these are not real domains
- **Respect established taxonomies**: If a well-known field classification \
exists (e.g., biology → botany → plant physiology), follow it
"""
)


# Entities have no ontology for now they are only relational nodes.


# ── EVENT ontology prompt ──────────────────────────────────────────────

EVENT_ONTOLOGY_PROMPT = (
    _SHARED_PREAMBLE
    + """
## Event Hierarchy Principles

Events are temporal occurrences — things that happened at a specific time \
or period. The root is "All Events".

**Event ancestry follows temporal and causal containment**, inspired by \
BFO's occurrent hierarchy. A parent event is the SMALLEST containing \
event or the most specific event category the child belongs to.

### Level patterns (from specific to general):

**Military & Conflict Events**
```
D-Day landings (June 6 1944)
  → Battle of Normandy
    → Western Front campaigns 1944
      → World War II European theater
        → World War II
          → 20th century global conflicts
            → all events

Battle of Gettysburg
  → American Civil War battles
    → American Civil War
      → 19th century American conflicts
        → American military history
          → all events

bombing of Hiroshima
  → atomic bombings of Japan 1945
    → Pacific War final campaigns
      → World War II Pacific theater
        → World War II
          → 20th century global conflicts
            → all events
```

**Scientific Discoveries & Experiments**
```
Aspect experiment 1982
  → Bell test experiments
    → quantum mechanics experiments
      → physics experiments
        → scientific experiments
          → all events

discovery of penicillin 1928
  → antibiotic discoveries
    → pharmaceutical discoveries
      → medical breakthroughs
        → scientific discoveries
          → all events

detection of gravitational waves 2015
  → LIGO observations
    → gravitational wave astronomy milestones
      → astrophysics breakthroughs
        → scientific discoveries
          → all events
```

**Political Events & Policy Decisions**
```
signing of the Treaty of Versailles 1919
  → Paris Peace Conference 1919
    → World War I peace settlements
      → post-war diplomatic events
        → diplomatic events
          → all events

January 6 Capitol riot 2021
  → 2020-2021 US election disputes
    → American political crises
      → American political events
        → political events
          → all events

fall of the Berlin Wall 1989
  → German reunification events
    → end of the Cold War events
      → Cold War events
        → 20th century geopolitical events
          → all events
```

**Disasters & Crises**
```
Chernobyl reactor explosion 1986
  → Chernobyl disaster
    → nuclear accidents
      → industrial disasters
        → disasters
          → all events

2008 Lehman Brothers collapse
  → 2008 financial crisis events
    → 2008 global financial crisis
      → financial crises
        → economic crises
          → all events

Deepwater Horizon oil spill 2010
  → offshore drilling accidents
    → oil industry disasters
      → environmental disasters
        → disasters
          → all events
```

**Awards, Prizes & Ceremonies**
```
2020 Nobel Prize in Chemistry (CRISPR)
  → Nobel Prizes in Chemistry
    → Nobel Prize awards
      → scientific awards
        → awards and honors
          → all events

Academy Award for Best Picture 2020
  → Academy Awards ceremonies
    → film industry awards
      → entertainment awards
        → awards and honors
          → all events
```

**Technological Milestones & Launches**
```
Apollo 11 moon landing 1969
  → Apollo 11 mission
    → Apollo program missions
      → NASA crewed spaceflight missions
        → spaceflight milestones
          → all events

launch of ChatGPT November 2022
  → large language model releases
    → artificial intelligence product launches
      → technology product launches
        → technology milestones
          → all events

first successful human-to-human heart transplant 1967
  → organ transplantation milestones
    → surgical milestones
      → medical milestones
        → scientific milestones
          → all events
```

**Legal Events & Court Decisions**
```
Brown v. Board of Education ruling 1954
  → US Supreme Court civil rights decisions
    → US Supreme Court landmark decisions
      → US legal milestones
        → legal events
          → all events

Nuremberg trials 1945-1946
  → post-World War II war crimes tribunals
    → international criminal tribunals
      → international legal proceedings
        → legal events
          → all events
```

**Social & Cultural Events**
```
Woodstock festival 1969
  → 1960s music festivals
    → music festivals
      → cultural events
        → all events

March on Washington 1963
  → American civil rights movement events
    → civil rights demonstrations
      → social movement events
        → social events
          → all events

first Earth Day 1970
  → environmental movement milestones
    → environmental activism events
      → social movement events
        → social events
          → all events
```

### Key rules for events:
- **Temporal containment first**: D-Day is part of the Battle of Normandy, \
which is part of the Western Front campaigns. This is the primary axis.
- **Category containment second**: If no direct temporal parent exists, \
use the most specific event category. "Discovery of penicillin" → \
"antibiotic discoveries" → "pharmaceutical discoveries"
- **Include dates in node names when useful**: "Aspect experiment 1982" \
is more specific and discoverable than "Aspect experiment"
- **Parent events must be real**: Do NOT invent parent events that didn't \
happen. "Western Front campaigns 1944" is real; "important things in \
1944" is not
- **Category ancestors use established terminology**: "nuclear accidents", \
"scientific discoveries", "financial crises" — these are recognized \
event categories, not ad-hoc groupings
- **Avoid over-nesting**: "Chernobyl reactor explosion" → "Chernobyl \
disaster" is valid (the explosion is part of the larger disaster). But \
do not add "bad things that happened at Chernobyl" as an intermediate \
level
"""
)


# ── PERSPECTIVE ontology prompt ────────────────────────────────────────

PERSPECTIVE_ONTOLOGY_PROMPT = (
    _SHARED_PREAMBLE
    + """
## Perspective Hierarchy Principles

Perspectives are debatable claims or positions — thesis/antithesis pairs \
built using Hegelian dialectics. Each perspective has a source concept it \
debates. The root is "All Perspectives".

**Perspective ancestry follows argumentative generalization** — each parent \
is the broader claim that the child claim specializes or specifies. The \
hierarchy goes from specific policy positions to general philosophical stances.

### Level patterns (from specific to general):

**Economic Policy Perspectives**
```
sugar taxes reduce childhood obesity rates
  → consumption taxes improve public health outcomes
    → government taxation as public health intervention
      → government intervention improves market outcomes
        → state economic intervention positions
          → all perspectives

universal basic income eliminates poverty traps
  → unconditional cash transfers improve welfare outcomes
    → direct cash transfers over in-kind benefits
      → welfare reform positions
        → social policy positions
          → all perspectives

rent control prevents displacement of vulnerable tenants
  → price controls protect consumers
    → market regulation positions
      → state economic intervention positions
        → all perspectives
```

**Science & Technology Ethics Perspectives**
```
germline editing to eliminate heritable diseases prevents lifetime suffering
  → genetic intervention to prevent disease is ethical
    → medical intervention before consent is justifiable for severe conditions
      → preventive medical ethics positions
        → bioethics positions
          → all perspectives

artificial general intelligence poses existential risk to humanity
  → advanced AI systems require strict safety constraints
    → technology risk management positions
      → technology governance positions
        → technology ethics positions
          → all perspectives

nuclear energy is the safest low-carbon power source per TWh
  → nuclear energy is a net benefit to society
    → nuclear energy positions
      → energy policy positions
        → all perspectives
```

**Political & Social Perspectives**
```
mass surveillance programs violate Fourth Amendment protections
  → government surveillance violates civil liberties
    → civil liberties should constrain state power
      → individual rights positions
        → political philosophy positions
          → all perspectives

mandatory vaccination for school enrollment protects herd immunity
  → public health mandates override individual medical choice
    → collective welfare justifies individual constraints
      → communitarian positions
        → political philosophy positions
          → all perspectives

free speech protections should extend to hate speech
  → speech restrictions cause more harm than the speech itself
    → free expression maximalist positions
      → individual rights positions
        → political philosophy positions
          → all perspectives
```

**Environmental Perspectives**
```
carbon emissions trading reduces pollution more efficiently than regulation
  → market mechanisms outperform regulation for environmental goals
    → market-based environmental policy positions
      → environmental policy positions
        → all perspectives

organic farming cannot feed the global population at scale
  → industrial agriculture is necessary for food security
    → agricultural intensification positions
      → food policy positions
        → environmental policy positions
          → all perspectives
```

**Historical & Interpretive Perspectives**
```
the Treaty of Versailles caused World War II
  → punitive peace treaties create conditions for future conflict
    → peace settlement effectiveness positions
      → international relations theory positions
        → geopolitical analysis positions
          → all perspectives

the Renaissance was primarily driven by economic factors not cultural ones
  → economic materialism drives cultural change
    → materialist historical interpretation positions
      → historiographical methodology positions
        → all perspectives
```

**Philosophy of Science Perspectives**
```
scientific paradigms are incommensurable (Kuhn)
  → scientific theories cannot be objectively compared across paradigms
    → scientific anti-realism positions
      → philosophy of science positions
        → epistemological positions
          → all perspectives

consciousness cannot be reduced to neural computation
  → subjective experience is irreducible to physical processes
    → anti-reductionist positions on consciousness
      → philosophy of mind positions
        → metaphysical positions
          → all perspectives
```

### Key rules for perspectives:
- **Argumentative generalization, not topic generalization**: The parent \
of "sugar taxes reduce obesity" is NOT "obesity" (that's the topic). \
It IS "consumption taxes improve public health" (the broader argument)
- **Both thesis and antithesis get separate ancestries**: They share the \
parent concept but have DIFFERENT argumentative lineages. "Sugar taxes \
reduce obesity" and "sugar taxes harm low-income families" both debate \
sugar taxes but belong to different argumentative traditions
- **Perspectives terminate at philosophical positions**: The highest \
ancestors are broad philosophical stances — "individual rights positions", \
"communitarian positions", "epistemological positions", "bioethics positions"
- **Maintain the claim structure**: Every ancestor should itself be a \
debatable position. "Government intervention improves market outcomes" \
is a valid ancestor because it's arguable. "Things about government" is not
- **Avoid topic leakage**: The ancestry is about the ARGUMENT lineage, \
not the SUBJECT lineage. "Nuclear energy is safe" → "nuclear energy \
positions" is topic leakage. "Nuclear energy is safe" → "nuclear energy \
is a net benefit" → "technology benefit assessment positions" follows \
the argumentative thread
- **Source concept is NOT an ancestor**: The source concept (the topic being \
debated) is linked via source_concept_id, not through the hierarchy. \
The hierarchy traces the argument's generalization path
"""
)


# ── Builder function ───────────────────────────────────────────────────

ONTOLOGY_PROMPTS: dict[str, str] = {
    "concept": CONCEPT_ONTOLOGY_PROMPT,
    "event": EVENT_ONTOLOGY_PROMPT,
    "perspective": PERSPECTIVE_ONTOLOGY_PROMPT,
}


def build_ontology_architect_user_msg(
    node_name: str,
    node_type: str,
    definition: str | None = None,
    existing_ancestors: list[str] | None = None,
    domain_context: str | None = None,
    dimension_snippets: list[str] | None = None,
) -> str:
    """Build the user message for the ontology architect LLM call.

    Args:
        node_name: The name of the node to classify.
        node_type: One of "concept", "event", "perspective".
        definition: Optional node definition (from dimensions or initial context).
        existing_ancestors: Names of ancestors already present in the graph that
            could serve as merge points. The LLM should try to connect to these
            where appropriate.
        domain_context: Optional domain hint (e.g., the research scope that
            created this node) to disambiguate polysemous terms.
        dimension_snippets: Optional dimension content snippets (from multi-model
            analysis) providing richer context about what the node covers.

    Returns:
        The user prompt string.
    """
    parts = [f'Node: "{node_name}" (type: {node_type})']

    if domain_context:
        parts.append(f"Domain context: {domain_context}")

    if definition:
        parts.append(f"Definition: {definition[:500]}")

    if dimension_snippets:
        parts.append(
            "Dimension analyses (from multi-model perspectives):\n" + "\n".join(f"  - {s}" for s in dimension_snippets)
        )

    if existing_ancestors:
        capped = existing_ancestors[:30]
        parts.append(
            "Existing ancestors in the graph (merge into these where appropriate):\n"
            + "\n".join(f"  - {a}" for a in capped)
        )

    root_names = {
        "concept": "all concepts",
        "event": "all events",
        "perspective": "all perspectives",
    }
    root = root_names.get(node_type, "all concepts")

    parts.append(
        f'\nPropose the complete ancestry chain from this node to the root "{root}". '
        "Include the node itself as the first element and the root as the last. "
        "Each intermediate ancestor should be a genuine parent — a domain, category, "
        "or containing concept that this node naturally belongs under. "
        "Do NOT chain sibling concepts through each other. "
        "Output ONLY the JSON array."
    )

    return "\n".join(parts)
