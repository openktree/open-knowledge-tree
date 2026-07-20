// Regression test for the wrapSentences reconcile that maps rendered
// HTML text characters back to raw-markdown rune offsets.
//
// The original bug: the greedy two-pointer matched a plain `\n` against
// a `\n` deep inside the first list item's body (because micromark
// collapses the heading + list opening whitespace differently from the
// raw markdown), racing the raw cursor ahead and making every
// subsequent plain char unfindable. The MAX_SKIP=256 cap then bailed
// and dropped the rest of the document — the user saw highlights
// disappear at the first list block in a report.
//
// This test feeds the exact failing fixture (a heading followed by a
// 6-item list block followed by a paragraph) and asserts that
// sentences AFTER the list block get wrapped in spans with
// data-sentence-index. Before the fix, no span after the list block
// existed.

// Minimal micromark render — pulled in dynamically so the test stays
// focused on wrapSentences behavior. Using the same micromark the app
// uses keeps the fixture realistic.
import { micromark } from "micromark";
import { gfm, gfmHtml } from "micromark-extension-gfm";
import { describe, expect, it } from "vitest";
import { segmentSentences } from "./sentences";
import { wrapSentencesHtml } from "./wrapSentences";

function render(md) {
  return micromark(String(md), { extensions: [gfm()], htmlExtensions: [gfmHtml()] });
}

describe("wrapSentencesHtml — list-block regression", () => {
  // The exact structural shape that triggered the original bail:
  // a heading, a prose sentence, a 6-item numbered list (each item is
  // a long paragraph), and a paragraph after the list. The list block
  // is one sentence unit in the chunker (contiguous list), so the
  // reconcile must map the rendered list text back to that one
  // sentence and continue past it to wrap the trailing paragraph.
  const md = `### 3.2 Cross-scope bridges (the value of integration)

The cross-pollination across scopes yields several integrations that no single scope surfaces on its own:

1. **The Hall RCT's eating-rate finding connects Scope 2 to Scope 3 and bears on Scope 4.** Hall 2019 found no appetite or glucose difference between arms — only eating rate differed. This pattern *supports* the texture/eating-rate mechanism (Scope 3) over the satiety-hormone or microbiome pathway.

2. **The Moli-sani 22.3% attenuation and the UK Biobank "diet quality predominates" finding connect Scope 1 to Scope 4.** These are the most direct observational tests of whether the UPF association survives diet-quality adjustment.

3. **The whole-grain-bread-and-yogurt-protective finding connects Scope 1 to Scope 4 and Scope 6.** Chen et al.'s three-cohort US T2DM analysis found whole-grain breads, yogurt, and dairy-based desserts (all NOVA-4 UPFs) associated with *lower* T2DM risk.

4. **The mechanism specificity finding (Scope 3) and the NOVA taxonomy critique (Scope 4) reinforce each other.** If each "processing-specific" mechanism reduces to a specific additive or property, and if the NOVA category is itself an unreliable classifier, then the case for category-level regulation weakens.

5. **The industry-funding analysis (Scope 5) and the policy analysis (Scope 6) intersect on the UK-advisers finding.** The 2024 *BMJ* documentation of UK government nutrition advisers receiving payments from Mars, Mondelēz, Nestlé, and PepsiCo is a concrete instance of the funding-outcome pattern.

6. **The symmetric missing meta-analysis (Scope 5) is the missing test for the policy debate (Scope 6).** The Touvier et al. 2023 BMJ "cannot wait" position and the DGAC/SACN "insufficient evidence" position are both readings of a literature produced by two funded research networks.

## 4. The Four Cross-Cutting Contested Questions

The skeptic case has the structure of confounding, the documented heterogeneity of NOVA4, and the residual confounding precedents on its side.
`;

  it("wraps sentences after the list block (the original bail point)", () => {
    const sentences = segmentSentences(md);
    const html = render(md);
    const out = wrapSentencesHtml(md, html, null, null);

    // Find the sentence index of the trailing paragraph "The skeptic
    // case has the structure of confounding". Before the fix, this
    // sentence was unmapped (rawOffsetOf = -1 for all its plain chars)
    // because the reconcile bailed at the list block.
    const target = "The skeptic case has the structure of confounding";
    const targetSentence = sentences.find((s) => s.text.includes(target));
    expect(targetSentence).toBeTruthy();

    // The wrapped HTML must contain a span with that sentence's index.
    const expectedAttr = `data-sentence-index="${targetSentence.index}"`;
    expect(out).toContain(expectedAttr);

    // And the span must wrap the actual rendered text of that sentence
    // (not just be an empty marker). Find the span and check it
    // contains "skeptic".
    const spanIdx = out.indexOf(expectedAttr);
    expect(spanIdx).toBeGreaterThan(-1);
    const spanSlice = out.slice(spanIdx, spanIdx + 400);
    expect(spanSlice).toContain("skeptic");
  });

  it("wraps the heading sentence before the list block", () => {
    const sentences = segmentSentences(md);
    const html = render(md);
    const out = wrapSentencesHtml(md, html, null, null);

    const target = "Cross-scope bridges";
    const targetSentence = sentences.find((s) => s.text.includes(target));
    expect(targetSentence).toBeTruthy();
    expect(out).toContain(`data-sentence-index="${targetSentence.index}"`);
  });

  it("every sentence the chunker produces is wrappable to at least one span", () => {
    // Property: for any markdown input, every chunk from segmentSentences
    // whose text appears (even partially) in the rendered plain text
    // must produce at least one <span data-sentence-index="N"> in the
    // wrapped output. This is the contract the UI relies on to
    // highlight annotated sentences.
    const sentences = segmentSentences(md);
    const html = render(md);
    const out = wrapSentencesHtml(md, html, null, null);

    // Skip sentences that are pure structural markers (e.g. a heading
    // whose text is just "#" runes) — their rendered text may be
    // empty. We check sentences whose text has at least one
    // alphanumeric char.
    for (const s of sentences) {
      if (!/[A-Za-z0-9]/.test(s.text)) continue;
      const attr = `data-sentence-index="${s.index}"`;
      expect(
        out,
        `sentence ${s.index} (${JSON.stringify(s.text.slice(0, 40))}) should produce a span`,
      ).toContain(attr);
    }
  });

  it("highlights annotated sentences and emits the count badge", () => {
    const sentences = segmentSentences(md);
    const html = render(md);
    // Mark the trailing paragraph sentence as having 3 facts.
    const target = "The skeptic case has the structure of confounding";
    const targetSentence = sentences.find((s) => s.text.includes(target));
    const highlightIndices = new Set([targetSentence.index]);
    const factCounts = new Map([[targetSentence.index, 3]]);
    const out = wrapSentencesHtml(md, html, highlightIndices, factCounts);

    expect(out).toContain("okt-sentence--has-facts");
    expect(out).toContain('data-sentence-index="' + targetSentence.index + '"');
    // The count <sup> lives inside the has-facts span.
    const hasFactsIdx = out.indexOf("okt-sentence--has-facts");
    const supIdx = out.indexOf("okt-sentence-count", hasFactsIdx);
    expect(supIdx).toBeGreaterThan(hasFactsIdx);
    expect(out.slice(supIdx, supIdx + 100)).toContain(">3<");
  });
});

describe("wrapSentencesHtml — structural whitespace cases", () => {
  it("handles a heading followed immediately by a list (no prose between)", () => {
    const md = `## Heading

- item one is long enough to pass the rune floor for sure
- item two is also long enough to pass the rune floor for sure

After the list, a paragraph sentence that must be wrapped.
`;
    const sentences = segmentSentences(md);
    const html = render(md);
    const out = wrapSentencesHtml(md, html, null, null);
    const target = sentences.find((s) => s.text.includes("After the list"));
    expect(target).toBeTruthy();
    expect(out).toContain(`data-sentence-index="${target.index}"`);
  });

  it("handles nested lists without dropping the tail", () => {
    const md = `Intro paragraph that sets up the list.

- top level item one is long enough
  - nested item under one is long enough too
  - another nested item under one
- top level item two is long enough

Trailing paragraph after the nested list to confirm the tail is wrapped.
`;
    const sentences = segmentSentences(md);
    const html = render(md);
    const out = wrapSentencesHtml(md, html, null, null);
    const target = sentences.find((s) => s.text.includes("Trailing paragraph"));
    expect(target).toBeTruthy();
    expect(out).toContain(`data-sentence-index="${target.index}"`);
  });

  it("handles blockquotes followed by prose", () => {
    const md = `> This is a quote that is long enough to pass the rune floor for the chunker to keep it.

After the quote, a normal prose sentence that must be wrapped correctly.
`;
    const sentences = segmentSentences(md);
    const html = render(md);
    const out = wrapSentencesHtml(md, html, null, null);
    const target = sentences.find((s) => s.text.includes("After the quote"));
    expect(target).toBeTruthy();
    expect(out).toContain(`data-sentence-index="${target.index}"`);
  });
});
