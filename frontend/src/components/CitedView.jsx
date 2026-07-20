import { createEffect, createMemo } from "solid-js";
import { renderMarkdown } from "../lib/markdown";
import { wrapSentencesHtml } from "../lib/wrapSentences";

export default function CitedView(props) {
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
    const html = renderMarkdown(md);
    return wrapSentencesHtml(md, html, hi(), fc());
  });

  let bodyEl;
  createEffect(() => {
    const html = wrappedHtml();
    if (bodyEl) bodyEl.innerHTML = html;
  });

  const handleClick = (e) => {
    const span = e.target.closest("span.okt-sentence--has-facts");
    if (!span) return;
    const idx = Number(span.dataset.sentenceIndex);
    if (Number.isFinite(idx)) props.onSentenceClick?.(idx);
  };

  return (
    <div
      class="prose dark:prose-invert max-w-none text-sm text-text-base leading-relaxed"
      ref={(el) => {
        bodyEl = el;
        if (el) el.innerHTML = wrappedHtml();
      }}
      onClick={handleClick}
    />
  );
}
