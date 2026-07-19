import { segmentSentences } from "./sentences";

/**
 * Wrap each sentence in the rendered markdown HTML in a
 * <span class="okt-sentence" data-sentence-index="N"> so the UI can
 * highlight sentences that have facts and wire click handlers.
 *
 * Strategy: the backend stores `sentence_index` values that index into
 * the RAW markdown (the same string `segmentSentences` runs on). We
 * run the same splitter here, producing sentences with raw-markdown
 * rune offsets. We then render the markdown to HTML via micromark,
 * parse it with DOMParser, walk text nodes, and for each rendered
 * character find the corresponding raw-markdown rune offset via a
 * greedy two-pointer reconcile. With that map, each rendered text-node
 * character is assigned to a sentence, and we wrap contiguous runs of
 * characters belonging to the same sentence in a <span>.
 *
 * Why the greedy reconcile: the rendered HTML text content is the raw
 * markdown with markdown marker characters (`*`, `_`, `[`, `]`, `(`, `)`,
 * leading `#`/`-`/`>`, image `!`, etc.) stripped. A previous version
 * of this file compared the raw sentence text (markers intact) to the
 * rendered text node (markers stripped) via `startsWith`, which failed
 * on the very first sentence containing any inline formatting. The
 * reconcile walk consumes each rendered character against the raw,
 * skipping any raw rune that does not match, so the map is correct
 * even when the raw sentence begins with `**bold**`, `_italic_`, a
 * link `[text](url)`, or a heading `#`.
 *
 * `highlightIndices` is a Set<number> of sentence_index values that
 * have at least one fact; those spans get the
 * `okt-sentence--has-facts` class so the UI can style them.
 *
 * `factCounts` is a Map<number, number> of sentence_index → fact
 * count. When present and a sentence is highlighted, a
 * `<sup class="okt-sentence-count">N</sup>` is appended inside the
 * LAST segment of that sentence's spans so the reader can see how
 * many facts came from that sentence. The `<sup>` lives inside the
 * `okt-sentence` span so the existing click handler
 * (closest("span.okt-sentence--has-facts")) still catches clicks on it.
 *
 * Returns the serialized HTML string (body.innerHTML) ready to
 * inject via el.innerHTML.
 */
export function wrapSentencesHtml(rawMarkdown, renderedHtml, highlightIndices, factCounts) {
  if (!rawMarkdown || !renderedHtml) return renderedHtml;
  const sentences = segmentSentences(rawMarkdown);
  if (!sentences.length) return renderedHtml;

  const doc = new DOMParser().parseFromString(renderedHtml, "text/html");
  const body = doc.body;

  // Collect text nodes in document order, skipping code/pre subtrees.
  const textNodes = [];
  collectTextNodes(body, textNodes);

  // Build plainText from text nodes and remember each text node's
  // [start, end) range within plainText.
  let plainText = "";
  const nodeRanges = [];
  for (const tn of textNodes) {
    const val = tn.nodeValue;
    const start = plainText.length;
    plainText += val;
    nodeRanges.push({ node: tn, start, end: plainText.length });
  }

  // Build rawOffsetOf[c] = raw markdown rune offset for plainText[c].
  // Greedy two-pointer: advance the raw cursor over marker runes
  // (anything that does not equal the next plain char). Cap the
  // consecutive skip count so a real divergence degrades to
  // "remainder unmapped" instead of looping forever.
  const raw = Array.from(rawMarkdown);
  const rawOffsetOf = new Int32Array(plainText.length).fill(-1);
  let r = 0;
  const MAX_SKIP = 256;
  for (let c = 0; c < plainText.length; c++) {
    const pc = plainText[c];
    let skipped = 0;
    while (r < raw.length && raw[r] !== pc) {
      if (raw[r] === "&") {
        // Try to decode an HTML entity in raw that resolves to `pc`.
        const ent = tryDecodeEntity(raw, r);
        if (ent.ch === pc) {
          rawOffsetOf[c] = r;
          r += ent.len;
          break;
        }
      }
      r++;
      if (++skipped > MAX_SKIP) {
        // Stop mapping; leave remaining plainText chars unmapped.
        c = plainText.length;
        break;
      }
    }
    if (c >= plainText.length) break;
    if (rawOffsetOf[c] === -1 && r < raw.length && raw[r] === pc) {
      rawOffsetOf[c] = r;
      r++;
    }
  }

  // Compute per-text-node segment boundaries: each text node is
  // partitioned into contiguous runs of characters that belong to the
  // same sentence (by raw offset). Each segment is either a real
  // sentence (sIdx >= 0) or an unmapped/gap region (sIdx === -1) that
  // we leave as plain text.
  const allSegments = []; // {node, start, end, sIdx, isLastOfSentence}
  for (const { node, start, end } of nodeRanges) {
    let curSIdx = -2; // sentinel meaning "no segment open yet"
    let segStart = 0;
    for (let i = 0; i < end - start; i++) {
      const roff = rawOffsetOf[start + i];
      const sIdx = roff < 0 ? -1 : findSentence(sentences, roff);
      if (sIdx !== curSIdx) {
        if (curSIdx >= 0) {
          allSegments.push({ node, start: segStart, end: i, sIdx: curSIdx });
        }
        curSIdx = sIdx;
        segStart = i;
      }
    }
    if (curSIdx >= 0) {
      allSegments.push({ node, start: segStart, end: end - start, sIdx: curSIdx });
    }
  }

  // Mark the last segment (in document order) of each sentence that
  // has facts as the one that gets the count badge appended.
  const lastSegBySentence = new Map();
  for (let k = 0; k < allSegments.length; k++) {
    lastSegBySentence.set(allSegments[k].sIdx, k);
  }
  const lastSegSet = new Set(lastSegBySentence.values());

  // Group segments by text node so we can wrap from the end backwards
  // (back-to-front wrapping preserves earlier offsets inside a node).
  const segmentsByNode = new Map();
  for (let k = 0; k < allSegments.length; k++) {
    const seg = allSegments[k];
    const list = segmentsByNode.get(seg.node) || [];
    list.push({ ...seg, isLast: lastSegSet.has(k) });
    segmentsByNode.set(seg.node, list);
  }

  for (const { node, start: nodeStart } of nodeRanges) {
    const segs = segmentsByNode.get(node);
    if (!segs || !segs.length) continue;
    // Wrap from highest localStart to lowest so each splitText's
    // prefix offsets remain valid on the live text node.
    const sorted = segs.slice().sort((a, b) => b.start - a.start);
    for (const seg of sorted) {
      const sentence = sentences[seg.sIdx];
      const hasFacts = highlightIndices && highlightIndices.has(sentence.index);
      const cls = hasFacts ? "okt-sentence okt-sentence--has-facts" : "okt-sentence";
      const span = wrapTextNodeRange(doc, node, seg.start, seg.end, cls, String(sentence.index));
      if (hasFacts && factCounts && factCounts.has(sentence.index) && seg.isLast) {
        const count = factCounts.get(sentence.index);
        const sup = doc.createElement("sup");
        sup.setAttribute("class", "okt-sentence-count");
        sup.setAttribute("data-sentence-index", String(sentence.index));
        sup.textContent = String(count);
        span.appendChild(sup);
      }
    }
    // nodeStart reserved for potential future chunked offset
    // accounting; currently unused.
    void nodeStart;
  }

  return body.innerHTML;
}

function collectTextNodes(root, out) {
  const walker = (node) => {
    if (node.nodeType === Node.TEXT_NODE) {
      out.push(node);
      return;
    }
    if (node.nodeType === Node.ELEMENT_NODE) {
      const tag = node.tagName.toLowerCase();
      if (tag === "code" || tag === "pre") return;
    }
    for (const c of Array.from(node.childNodes)) walker(c);
  };
  walker(root);
}

function findSentence(sentences, roff) {
  // Binary search for the sentence whose [start, end) contains roff.
  // Sentences are sorted by start; gaps (blank lines, code/heading/list
  // units we still cover, but raw offsets in blank lines belong to no
  // sentence) return -1.
  let lo = 0;
  let hi = sentences.length - 1;
  let ans = -1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    if (sentences[mid].start <= roff) {
      ans = mid;
      lo = mid + 1;
    } else {
      hi = mid - 1;
    }
  }
  if (ans < 0) return -1;
  return roff < sentences[ans].end ? ans : -1;
}

function tryDecodeEntity(raw, at) {
  // Recognize named/numeric HTML entities at raw[at..] and return
  // { ch: decoded-first-char, len: total-runes-consumed } or
  // { ch: "", len: 0 } if not an entity. Only the first decoded
  // character is needed because the reconcile only consumes one
  // plainText char per iteration; the rest are re-matched naturally
  // when the reconcile reaches them.
  if (raw[at] !== "&") return { ch: "", len: 0 };
  let i = at + 1;
  while (i < raw.length && raw[i] !== ";" && i - at < 16) i++;
  if (i >= raw.length || raw[i] !== ";") return { ch: "", len: 0 };
  const name = raw.slice(at + 1, i).join("");
  const decoded = decodeEntity(name);
  if (!decoded) return { ch: "", len: 0 };
  return { ch: decoded[0], len: i - at + 1 };
}

function decodeEntity(name) {
  if (!name) return "";
  if (name[0] === "#") {
    const code =
      name[1] === "x" || name[1] === "X"
        ? parseInt(name.slice(2), 16)
        : parseInt(name.slice(1), 10);
    if (!Number.isFinite(code)) return "";
    try {
      return String.fromCodePoint(code);
    } catch {
      return "";
    }
  }
  const named = {
    amp: "&",
    lt: "<",
    gt: ">",
    quot: '"',
    apos: "'",
    nbsp: "\u00a0",
    ndash: "\u2013",
    mdash: "\u2014",
    hellip: "\u2026",
    laquo: "\u00ab",
    raquo: "\u00bb",
  };
  return named[name.toLowerCase()] || "";
}

function wrapTextNodeRange(doc, textNode, start, end, className, sentenceIndex) {
  // Splits textNode at [start, end) and wraps the middle segment in a
  // span. Uses splitText directly (instead of Range.surroundContents)
  // because surroundContents invalidates the textNode reference in
  // ways that are hard to reason about when wrapping multiple
  // segments in the same node — we need a stable handle on the
  // returned span to append the count <sup> afterwards.
  //
  // splitText(end) first (if end < length) keeps textNode = [0,end)
  // and returns suffix = [end,...). Then splitText(start) gives
  // textNode = [0,start) and middle = [start,end). We move middle into
  // the span and insertBefore(span, textNode.nextSibling) so the span
  // lands where middle was (right after textNode), preserving
  // document order across multiple wraps in the same node.
  const suffix = end < textNode.nodeValue.length ? textNode.splitText(end) : null;
  const middle = textNode.splitText(start); // textNode=[0,start), middle=[start,end)
  const span = doc.createElement("span");
  span.setAttribute("class", className);
  span.setAttribute("data-sentence-index", sentenceIndex);
  span.appendChild(middle);
  textNode.parentNode.insertBefore(span, textNode.nextSibling);
  void suffix; // suffix is only used to ensure the split landed; not referenced again
  return span;
}
