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
  //
  // Strategy: whitespace-aware greedy two-pointer with re-anchor. The
  // plain text is the rendered HTML's text-node content with tags
  // stripped, so it is the raw markdown with marker runes (`#`, `*`,
  // `_`, `[`, `]`, `(`, `)`, `>`, leading `1. ` list markers, etc.)
  // removed. micromark also collapses structural whitespace: a heading
  // followed by a blank line followed by a list opening (`### H\n\n1.
  // **T...`) renders to plain text where the marker runes are gone and
  // the whitespace runs are re-grouped (`H\n\n\nT...`). A naive
  // char-by-char greedy matcher matches the plain `\n` against the
  // first `\n` it finds in raw, which can be deep inside the list
  // item's body — racing the raw cursor ahead and making every
  // subsequent plain char unfindable. A previous version capped the
  // skip at 256 runes and bailed, which silently dropped the rest of
  // the document (the user saw highlights disappear at the first list
  // block in a report — see the wrapSentences regression test for the
  // exact fixture).
  //
  // The algorithm here:
  //   1. Collapse runs of whitespace (spaces, tabs, newlines) in BOTH
  //      plain and raw to a single canonical boundary marker before
  //      comparing, so a plain `\n\n\n` can match a raw `\n\n1. **`
  //      (the list marker runes between the newlines are skipped as
  //      non-whitespace markers, and the whitespace run as a whole
  //      consumes all whitespace runes in raw between the surrounding
  //      non-whitespace tokens).
  //   2. For non-whitespace plain chars, greedy-skip raw marker runes
  //      (anything that does not equal the plain char) up to a soft
  //      cap, then re-anchor: scan forward in raw for the next
  //      occurrence whose preceding context matches the preceding
  //      plain context. Pick the smallest-skip candidate that passes.
  //   3. If no re-anchor is found, mark this plain char as a gap
  //      (`rawOffsetOf[c] = -1`) and advance c only, leaving r where
  //      it is so the next plain char can re-anchor. The walker thus
  //      never bails and never drops the tail of the document.
  const raw = Array.from(rawMarkdown);
  const rawOffsetOf = new Int32Array(plainText.length).fill(-1);
  let r = 0;
  const SOFT_SKIP = 64; // runes to skip greedily before re-anchoring
  const REANCHOR_WINDOW = 4096; // max forward scan when re-anchoring
  const CONTEXT_K = 4; // previous plain chars used for context check
  const CONTEXT_WINDOW = 32; // raw runes before candidate to check context
  const isWS = (ch) => ch === " " || ch === "\t" || ch === "\n" || ch === "\r";

  // Helper: advance r to the next non-whitespace raw rune, returning
  // the list of whitespace rune offsets consumed (so we can record
  // where the leading whitespace plain char maps to, if needed). If the
  // raw cursor is already at a non-whitespace rune, returns [].
  const skipRawWS = (fromR) => {
    const ws = [];
    let rr = fromR;
    while (rr < raw.length && isWS(raw[rr])) {
      ws.push(rr);
      rr++;
    }
    return { next: rr, ws };
  };

  for (let c = 0; c < plainText.length; c++) {
    const pc = plainText[c];

    // Whitespace plain char: consume the ENTIRE raw whitespace run
    // starting at r (collapsing structural whitespace). Record the
    // first raw whitespace rune as the offset for this plain char;
    // subsequent plain whitespace chars in the same run map to -1
    // (gap) since the raw whitespace was already consumed. This is
    // what makes `own:\n\n\nThe` plain match `own:\n\n1. **The` raw:
    // the plain `\n`s collapse against the raw `\n`s, and the `1. **`
    // markers between them are skipped as non-whitespace before the
    // next non-whitespace plain char (`T`) is matched.
    if (isWS(pc)) {
      // If raw[r] is already non-whitespace, the plain whitespace run
      // has no raw whitespace to consume (micromark inserted it); mark
      // gap and move on.
      if (r >= raw.length || !isWS(raw[r])) {
        rawOffsetOf[c] = -1;
        continue;
      }
      // Consume the entire raw whitespace run; record the first rune.
      rawOffsetOf[c] = r;
      r++;
      while (r < raw.length && isWS(raw[r])) r++;
      // Any following plain whitespace chars in the same run are gaps.
      while (c + 1 < plainText.length && isWS(plainText[c + 1])) {
        c++;
        rawOffsetOf[c] = -1;
      }
      continue;
    }

    // Non-whitespace plain char. Skip raw marker runes (which may be
    // whitespace-bounded structural markers like `1.`, `**`, `#`) up
    // to SOFT_SKIP. If we hit raw whitespace while looking for a
    // non-whitespace match, that's fine — keep skipping.
    // Fast path: raw[r] already matches.
    if (r < raw.length && raw[r] === pc) {
      rawOffsetOf[c] = r;
      r++;
      continue;
    }
    if (r < raw.length && raw[r] === "&") {
      const ent = tryDecodeEntity(raw, r);
      if (ent.ch === pc) {
        rawOffsetOf[c] = r;
        r += ent.len;
        continue;
      }
    }

    // Greedy skip up to SOFT_SKIP runes (across whitespace).
    let skipped = 0,
      foundR = -1;
    while (r + skipped < raw.length && skipped < SOFT_SKIP) {
      const rr = r + skipped;
      if (raw[rr] === pc) {
        foundR = rr;
        break;
      }
      if (raw[rr] === "&") {
        const ent = tryDecodeEntity(raw, rr);
        if (ent.ch === pc) {
          foundR = rr;
          break;
        }
      }
      skipped++;
    }
    if (foundR >= 0) {
      rawOffsetOf[c] = foundR;
      r = foundR + 1;
      continue;
    }

    // Re-anchor: scan forward in raw for the next occurrence of pc
    // whose preceding context matches the preceding plain context.
    const ctxStart = Math.max(0, c - CONTEXT_K);
    const ctxChars = [];
    for (let k = ctxStart; k < c; k++) {
      // Skip gap chars in context (they carry no raw offset info).
      if (rawOffsetOf[k] >= 0) ctxChars.push(plainText[k]);
    }

    let bestR = -1;
    const scanLimit = Math.min(raw.length, r + REANCHOR_WINDOW);
    for (let rr = r + SOFT_SKIP; rr < scanLimit; rr++) {
      let matches = raw[rr] === pc;
      if (!matches && raw[rr] === "&") {
        const ent = tryDecodeEntity(raw, rr);
        if (ent.ch === pc) matches = true;
      }
      if (!matches) continue;
      if (ctxChars.length === 0) {
        bestR = rr;
        break;
      }
      // Context check: the last CONTEXT_K plain chars (with raw
      // offsets) should appear in raw within CONTEXT_WINDOW runes
      // before rr, in order.
      let ctxOK = true;
      let rawProbe = rr - 1;
      for (let k = ctxChars.length - 1; k >= 0; k--) {
        const need = ctxChars[k];
        let found = false;
        const probeFloor = Math.max(0, rawProbe - CONTEXT_WINDOW);
        while (rawProbe >= probeFloor) {
          if (raw[rawProbe] === need) {
            found = true;
            rawProbe--;
            break;
          }
          if (raw[rawProbe] === "&") {
            const ent = tryDecodeEntity(raw, rawProbe);
            if (ent.ch === need) {
              found = true;
              rawProbe -= ent.len;
              break;
            }
          }
          rawProbe--;
        }
        if (!found) {
          ctxOK = false;
          break;
        }
      }
      if (ctxOK) {
        bestR = rr;
        break;
      }
    }

    if (bestR >= 0) {
      rawOffsetOf[c] = bestR;
      r = bestR + 1;
      continue;
    }

    // No re-anchor found. Mark gap and advance c only (recovery).
    rawOffsetOf[c] = -1;
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
