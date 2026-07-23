import { createEffect, createMemo, createSignal } from "solid-js";
import { renderMarkdown } from "../lib/markdown";
import { normalizeCitations, normalizeImageCitations } from "../lib/normalizeCitations";
import { wrapSentencesHtml } from "../lib/wrapSentences";
import CitationModal from "./CitationModal";

// CitedView renders a cited markdown body (a report's body_md or a
// source's parsed_markdown) with two transformations applied BEFORE
// micromark:
//
//   1. normalizeImageCitations — rewrites ![alt](<fact:uuid>) image
//      embeds. When an imageMap prop is supplied (fact_id -> renderable
//      url), the embed becomes ![alt](url); without one the embed is
//      replaced with an italic *alt* placeholder rather than a broken
//      <img> (micromark treats <fact:uuid> as an invalid URL and would
//      otherwise emit a dead image).
//   2. normalizeCitations — rewrites [text](<fact:uuid>) and
//      [name](<concept:uuid>) (and the doubled ([fact:short](<fact:full>))
//      form the agentic synthesizer emits) into real markdown links to
//      /{slug}/facts/{id} or /{slug}/concepts/{id}. Without this step
//      every citation renders as <a href="">text</a> — a dead link —
//      because plain micromark rejects the angle-bracketed <fact:uuid>
//      destination as an invalid URL.
//
// The SAME normalized string is fed to both renderMarkdown and
// wrapSentencesHtml so the sentence-offset map (computed from the raw
// markdown) lines up with the rendered HTML.
//
// Citation clicks open an inline CitationModal (fact text + badges or
// concept definition preview) instead of navigating away — the reader
// stays in the report/synthesis context. Each modal carries a
// "View full … page →" link for the complete detail page.
//
// Props:
//   - markdown:      the raw cited markdown
//   - slug:          repo slug, used to build fact/concept detail hrefs
//   - imageMap:      optional Map<fact_id, renderableUrl> for image embeds
//   - annotations / highlightIndices / factCounts / onSentenceClick:
//     sentence-highlight wiring (see original CitedView).
export default function CitedView(props) {
  const slug = () => props.slug || "";

  // Active inline-citation modal state. `cite` is null when no
  // citation modal is open; otherwise { kind, id }.
  const [cite, setcite] = createSignal(null);
  const closeCite = () => setcite(null);

  const hi = createMemo(() => {
    if (props.highlightIndices) return props.highlightIndices;
    if (!props.annotations?.length) return null;
    const set = new Set();
    for (const a of props.annotations) set.add(a.sentence_index);
    return set;
  });

  const fc = createMemo(() => {
    if (props.factCounts) return props.factCounts;
    if (!props.annotations?.length) return null;
    const map = new Map();
    for (const a of props.annotations) {
      map.set(a.sentence_index, (map.get(a.sentence_index) || 0) + 1);
    }
    return map;
  });

  const wrappedHtml = createMemo(() => {
    const md = props.markdown || "";
    if (!md.trim()) return "";
    // Normalize image embeds first (with an empty map the embed becomes
    // a visible *alt* placeholder; with a real map it becomes a link).
    const imageMap = props.imageMap instanceof Map ? props.imageMap : new Map();
    let normalized = normalizeImageCitations(md, imageMap);
    normalized = normalizeCitations(normalized, slug());
    const html = renderMarkdown(normalized);
    // Sentence offsets must key off the same normalized string the
    // renderer saw, otherwise the reconcile walker misaligns.
    return wrapSentencesHtml(normalized, html, hi(), fc());
  });

  let bodyEl;
  createEffect(() => {
    const html = wrappedHtml();
    if (bodyEl) bodyEl.innerHTML = html;
  });

  const handleClick = (e) => {
    // Intercept internal fact/concept detail links and open an inline
    // CitationModal instead of navigating away (the reader stays in
    // the report/synthesis context). The modal carries a "View full
    // … page →" link for the complete detail page.
    const a = e.target.closest("a");
    if (a) {
      const href = a.getAttribute("href") || "";
      if (href.startsWith("/")) {
        const factM = href.match(/\/facts\/([0-9a-fA-F-]{36})/);
        if (factM) {
          e.preventDefault();
          setcite({ kind: "fact", id: factM[1] });
          return;
        }
        const conceptM = href.match(/\/concepts\/([0-9a-fA-F-]{36})/);
        if (conceptM) {
          e.preventDefault();
          setcite({ kind: "concept", id: conceptM[1] });
          return;
        }
      }
    }
    const span = e.target.closest("span.okt-sentence--has-facts");
    if (!span) return;
    const idx = Number(span.dataset.sentenceIndex);
    if (Number.isFinite(idx)) props.onSentenceClick?.(idx);
  };

  return (
    <>
      <div
        class="prose dark:prose-invert max-w-none text-sm text-text-base leading-relaxed"
        ref={(el) => {
          bodyEl = el;
          if (el) el.innerHTML = wrappedHtml();
        }}
        onClick={handleClick}
      />
      <CitationModal
        open={cite() != null}
        onClose={closeCite}
        kind={cite()?.kind}
        id={cite()?.id}
        slug={slug()}
      />
    </>
  );
}
