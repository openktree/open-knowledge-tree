import { segmentSentences } from "./sentences";

/**
 * Build a cited copy of a report: the original markdown body with
 * inline [N] citation markers after sentences that have supporting
 * facts, followed by an annex listing each fact's full text and the
 * URLs of its sources so they can be read and cross-checked.
 *
 * @param {string} bodyMd - raw markdown body
 * @param {Array} annotations - report annotation rows (each has
 *   sentence_index, fact_id, text, score). Already ordered by
 *   sentence_index then score DESC by the backend query.
 * @param {Map<string, Array<{url: string, parsed_title: string}>>} factSources
 *   fact_id -> source list (from api.getFact). May be partial; missing
 *   entries render with "(sources unavailable)".
 * @param {Map<string, {text: string, sources: Array}>} [inlineFacts]
 *   fact_id -> {text, sources} for facts the author cited inline in
 *   the prose (via [text](<fact:uuid>) links) that are NOT already
 *   in the auto-annotations. These are appended to the annex with a
 *   "(direct cite)" label so the reader sees the author's explicit
 *   citations alongside the auto-matched ones.
 * @returns {string} the cited markdown text
 *
 * Posture (supports / contradicts / related), when present on an
 * annotation, is surfaced two ways:
 *   - inline, as a superscript-ish tag after the citation marker
 *     (e.g. `[1:supp]`, `[2:contr]`, `[3:rel]`) so the reader sees the
 *     relationship without scrolling;
 *   - in the annex, as a parenthesized label before each fact's text.
 *
 * Inline (direct) citations carry a `direct` tag (e.g. `[4:direct]`)
 * to distinguish the author's explicit links from auto-matched facts.
 */
export function buildCitedText(bodyMd, annotations, factSources, inlineFacts) {
  if (!bodyMd) return "";

  const anns = annotations || [];
  const inline = inlineFacts instanceof Map ? inlineFacts : new Map();

  // Group fact_ids by sentence_index, preserving first-seen order
  // (annotations arrive sorted by sentence_index, score DESC).
  const factsBySentence = new Map();
  for (const a of anns) {
    const arr = factsBySentence.get(a.sentence_index) || [];
    if (!arr.includes(a.fact_id)) arr.push(a.fact_id);
    factsBySentence.set(a.sentence_index, arr);
  }

  // The set of fact_ids already covered by auto-annotations — inline
  // facts that also appear here are NOT duplicated in the annex.
  const annotatedIds = new Set(anns.map((a) => a.fact_id));

  // Assign citation numbers in order of first appearance in the text.
  // Auto-annotated facts are numbered first (by sentence order), then
  // inline-only facts (those in `inline` but not in annotatedIds) are
  // numbered after, in their first-appearance order in the inline map.
  const citationNumber = new Map();
  let nextNum = 1;
  const sentences = segmentSentences(bodyMd);
  for (const s of sentences) {
    const fids = factsBySentence.get(s.index);
    if (!fids) continue;
    for (const fid of fids) {
      if (!citationNumber.has(fid)) citationNumber.set(fid, nextNum++);
    }
  }
  // Inline-only facts get numbers after the annotated ones.
  const inlineOnlyIds = [];
  for (const fid of inline.keys()) {
    if (!annotatedIds.has(fid) && !citationNumber.has(fid)) {
      citationNumber.set(fid, nextNum++);
      inlineOnlyIds.push(fid);
    }
  }

  // Map each annotation's posture to a short inline tag so the
  // relationship is visible next to the citation marker without
  // forcing the reader down to the annex.
  const postureTag = { supports: "supp", contradicts: "contr", related: "rel" };
  const postureByFact = new Map();
  for (const a of anns) {
    if (a.posture && !postureByFact.has(a.fact_id)) {
      postureByFact.set(a.fact_id, a.posture);
    }
  }
  // Inline-only facts carry a "direct" posture label so the annex
  // distinguishes the author's explicit citations from auto-matched
  // facts.
  for (const fid of inlineOnlyIds) {
    if (!postureByFact.has(fid)) postureByFact.set(fid, "direct");
  }

  // Insert citation markers right after each cited sentence's terminal
  // punctuation (before its trailing whitespace). Work on a rune array
  // and splice from the end backwards so earlier offsets stay valid.
  // Only auto-annotated facts get inline markers (they map to a
  // sentence_index); inline-only facts are cited via the author's own
  // [text](<fact:uuid>) links in the prose, so they don't need an
  // injected marker.
  const runes = Array.from(bodyMd);
  const insertions = [];
  for (const s of sentences) {
    const fids = factsBySentence.get(s.index);
    if (!fids || !fids.length) continue;
    const marker = fids
      .map((fid) => {
        const num = citationNumber.get(fid);
        const tag = postureTag[postureByFact.get(fid)];
        return tag ? `[${num}:${tag}]` : `[${num}]`;
      })
      .join("");
    let trimEnd = s.text.length;
    while (trimEnd > 0 && /\s/.test(s.text[trimEnd - 1])) trimEnd--;
    insertions.push({ offset: s.start + trimEnd, text: marker });
  }
  insertions.sort((a, b) => b.offset - a.offset);
  for (const ins of insertions) {
    runes.splice(ins.offset, 0, ...Array.from(ins.text));
  }
  let output = runes.join("");

  // Annex: each fact's full text + source URLs, ordered by citation
  // number so the reader can cross-check [1], [2], ... in sequence.
  if (citationNumber.size > 0) {
    output += "\n\n---\n\n## Annex: Supporting Facts\n\n";
    const factTextById = new Map();
    for (const a of anns) {
      if (!factTextById.has(a.fact_id)) factTextById.set(a.fact_id, a.text || "");
    }
    // Inline-only fact text comes from the inlineFacts map.
    for (const [fid, info] of inline) {
      if (!factTextById.has(fid) && info?.text) factTextById.set(fid, info.text);
    }
    const sorted = [...citationNumber.entries()].sort((a, b) => a[1] - b[1]);
    for (const [fid, num] of sorted) {
      const factText = factTextById.get(fid) || "";
      const sources = factSources?.get(fid) || inline.get(fid)?.sources || [];
      const posture = postureByFact.get(fid);
      const postureLabel = posture ? ` (${posture})` : "";
      output += `[${num}]${postureLabel} ${factText}\n`;
      if (sources.length > 0) {
        output += "    Sources:\n";
        for (const src of sources) {
          output += `    - ${src.url}\n`;
        }
      } else {
        output += "    Sources: (unavailable)\n";
      }
      output += "\n";
    }
  }

  return output;
}
