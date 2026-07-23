// Unit tests for normalizeCitations. These lock in the citation
// forms the two synthesis systems emit and the frontend must rewrite
// before micromark. The critical regression: the agentic synthesizer
// emits a DOUBLED form ([fact:short](<fact:full>)) that plain micromark
// renders as <a href=""> — a dead link. normalizeCitations must
// rewrite it into a real route.
import { describe, expect, it } from "vitest";
import { normalizeCitations } from "./normalizeCitations.js";

const SLUG = "default";
const FACT = "03bbda2f-9fb1-43d8-8be8-e4db821a007e";
const CONCEPT = "5cd9e0e7-c8cb-4038-aa19-92813ba76227";

describe("normalizeCitations — canonical prefixed forms", () => {
  it("rewrites [text](<fact:uuid>) keeping the text and routing to /facts", () => {
    const out = normalizeCitations(`see [note](<fact:${FACT}>) here`, SLUG);
    expect(out).toBe(`see [note](/${SLUG}/facts/${FACT}) here`);
  });

  it("rewrites [name](<concept:uuid>) routing to /concepts", () => {
    const out = normalizeCitations(`see [Relation Extraction](<concept:${CONCEPT}>) here`, SLUG);
    expect(out).toBe(`see [Relation Extraction](/${SLUG}/concepts/${CONCEPT}) here`);
  });
});

describe("normalizeCitations — doubled agentic form", () => {
  // The form found in the OKT system paper report (id
  // fb4d0fb7-1691-428f-b030-fac4c526a044). Every citation in that
  // report used this shape; before the CitedView wiring fix all 337
  // rendered as <a href=""> dead links. The inner label is the
  // kind-prefixed short id ("fact:03bbda2f") which is not human prose,
  // so it is replaced with a numeric [N] reference.
  it("rewrites ([fact:short](<fact:full>)) to a numeric ref inside parens", () => {
    const src = `indexed ([fact:03bbda2f](<fact:${FACT}>)). next`;
    const out = normalizeCitations(src, SLUG);
    expect(out).toBe(`indexed ([1](/${SLUG}/facts/${FACT})). next`);
  });

  it("rewrites a doubled concept citation to a numeric ref", () => {
    const src = `([concept:5cd9e0e7](<concept:${CONCEPT}>)) frames`;
    const out = normalizeCitations(src, SLUG);
    expect(out).toBe(`([1](/${SLUG}/concepts/${CONCEPT})) frames`);
  });

  it("keeps the same number on a repeated id-style citation", () => {
    const src = `first ([fact:03bbda2f](<fact:${FACT}>)) and again ([fact:03bbda2f](<fact:${FACT}>))`;
    const out = normalizeCitations(src, SLUG);
    expect(out).toBe(`first ([1](/${SLUG}/facts/${FACT})) and again ([1](/${SLUG}/facts/${FACT}))`);
  });

  it("rewrites each link in a doubled comma-group to numeric refs", () => {
    const f1 = "b8febaee-d803-4705-a601-f57379c16cad";
    const f2 = "ca726d48-cf2a-4bfa-8ce7-872aa60f8541";
    const src = `([fact:b8febaee](<fact:${f1}>), [fact:ca726d48](<fact:${f2}>))`;
    const out = normalizeCitations(src, SLUG);
    expect(out).toBe(`([1](/${SLUG}/facts/${f1}), [2](/${SLUG}/facts/${f2}))`);
  });
});

describe("normalizeCitations — legacy bare-uuid forms", () => {
  it("rewrites [text](<uuid>) as a fact citation", () => {
    const out = normalizeCitations(`see [note](<${FACT}>) here`, SLUG);
    expect(out).toBe(`see [note](/${SLUG}/facts/${FACT}) here`);
  });

  it("rewrites a bare [uuid] as a numbered fact link", () => {
    const out = normalizeCitations(`see [${FACT}] here`, SLUG);
    expect(out).toBe(`see [1](/${SLUG}/facts/${FACT}) here`);
  });
});

describe("normalizeCitations — idempotent on already-normalized text", () => {
  it("does not double-rewrite a normalized link", () => {
    const src = `see [note](/${SLUG}/facts/${FACT}) here`;
    expect(normalizeCitations(src, SLUG)).toBe(src);
  });
});
