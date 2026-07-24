import { segmentSentences } from "./sentences";

// Build a cited copy of a report: the original markdown body with
// inline [N] citation markers after sentences that have auto-matched
// facts, the author's own [text](<fact:uuid>) links replaced with
// [D{M}] direct-cite markers, and an annex listing each fact's full
// text + source URLs so they can be read and cross-checked.
//
// Args:
//   - bodyMd:       raw markdown body
//   - annotations:  report annotation rows (each has sentence_index,
//                   fact_id, text, score, posture)
//   - factSources:  Map<fact_id, source[]> for annotated facts
//                   (from api.getFact). May be partial.
//   - inlineFacts:  Map<fact_id, {text, sources, error}> for facts the
//                   author cited inline that are NOT already in the
//                   annotations. `error` is set when the fetch failed
//                   (404, network, etc.); the annex records it so the
//                   reader sees the citation was attempted.
//
// Posture (supports/contradicts/related) is surfaced as a short inline
// tag ([1:supp], [2:contr], [3:rel]). Direct citations carry the
// [D{M}] form (D1, D2, …) so the reader distinguishes the author's
// explicit links from the auto-matched facts at a glance. The annex
// labels direct cites "(direct cite)" and records fetch errors as
// "(direct cite — unavailable: <error>)".
export function buildCitedText(bodyMd, annotations, factSources, inlineFacts) {
  if (!bodyMd) return "";

  const anns = annotations || [];
  const inline = inlineFacts instanceof Map ? inlineFacts : new Map();

  // Group auto-annotated fact_ids by sentence_index, preserving
  // first-seen order (annotations arrive sorted by sentence_index,
  // score DESC).
  const factsBySentence = new Map();
  for (const a of anns) {
    const arr = factsBySentence.get(a.sentence_index) || [];
    if (!arr.includes(a.fact_id)) arr.push(a.fact_id);
    factsBySentence.set(a.sentence_index, arr);
  }

  // The set of fact_ids already covered by auto-annotations — inline
  // facts that also appear here are NOT duplicated in the annex.
  const annotatedIds = new Set(anns.map((a) => a.fact_id));

  // Assign auto-citation numbers [N] in order of first appearance in
  // the text (by sentence order).
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

  // Assign direct-cite numbers [D{M}] to inline-only facts (those in
  // `inline` but not in annotatedIds), in first-appearance order in
  // the inline map.
  const directNumber = new Map();
  let nextDirect = 1;
  for (const fid of inline.keys()) {
    if (!annotatedIds.has(fid) && !citationNumber.has(fid) && !directNumber.has(fid)) {
      directNumber.set(fid, nextDirect++);
    }
  }

  // Map each annotation's posture to a short inline tag so the
  // relationship is visible next to the citation marker.
  const postureTag = { supports: "supp", contradicts: "contr", related: "rel" };
  const postureByFact = new Map();
  for (const a of anns) {
    if (a.posture && !postureByFact.has(a.fact_id)) {
      postureByFact.set(a.fact_id, a.posture);
    }
  }

  // Replace the author's inline [text](<fact:uuid>) / [text](<uuid>)
  // links with [D{M}] markers (only for facts that got a direct
  // number — facts already in the annotations keep their existing
  // links and are NOT replaced; they're covered by the [N] markers
  // injected at sentence ends). We replace the whole markdown link
  // (text + target) with the marker so the cited copy doesn't carry
  // raw UUIDs.
  const UUID =
    "[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}";
  const directLinkRe = new RegExp(`\\[[^\\]]*\\]\\(<\\s*(?:fact\\s*:\\s*)?(${UUID})\\s*>\\)`, "g");
  const outputBody = bodyMd.replace(directLinkRe, (full, id) => {
    const d = directNumber.get(id);
    if (d != null) return `[D${d}]`;
    return full;
  });

  // Insert auto-citation markers [N] right after each cited
  // sentence's terminal punctuation (before its trailing whitespace).
  // Work on a rune array and splice from the end backwards so earlier
  // offsets stay valid. (Sentences are re-segmented from the
  // link-replaced body so the offsets line up.)
  const runes = Array.from(outputBody);
  const replacedSentences = segmentSentences(outputBody);
  const insertions = [];
  for (const s of replacedSentences) {
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

  // Annex: each fact's full text + source URLs. Auto-annotated facts
  // first (ordered by [N]), then direct cites (ordered by [D{M}]),
  // each labeled so the reader distinguishes them.
  if (citationNumber.size > 0 || directNumber.size > 0) {
    output += "\n\n---\n\n## Annex: Supporting Facts\n\n";
    const factTextById = new Map();
    for (const a of anns) {
      if (!factTextById.has(a.fact_id)) factTextById.set(a.fact_id, a.text || "");
    }

    // Auto-annotated facts.
    const sortedAuto = [...citationNumber.entries()].sort((a, b) => a[1] - b[1]);
    for (const [fid, num] of sortedAuto) {
      const factText = factTextById.get(fid) || "";
      const sources = factSources?.get(fid) || [];
      const posture = postureByFact.get(fid);
      const postureLabel = posture ? ` (${posture})` : "";
      output += `[${num}]${postureLabel} ${factText}\n`;
      output += formatSources(sources);
      output += "\n";
    }

    // Direct cites.
    const sortedDirect = [...directNumber.entries()].sort((a, b) => a[1] - b[1]);
    for (const [fid, num] of sortedDirect) {
      const info = inline.get(fid) || {};
      const factText = info.text || "";
      const sources = info.sources || [];
      const err = info.error;
      let label = "(direct cite)";
      if (err) label = `(direct cite — unavailable: ${err})`;
      output += `[D${num}]${label} ${factText}\n`;
      output += formatSources(sources);
      output += "\n";
    }
  }

  return output;
}

function formatSources(sources) {
  if (!sources || sources.length === 0) {
    return "    Sources: (unavailable)\n";
  }
  let s = "    Sources:\n";
  for (const src of sources) {
    s += `    - ${src.url}\n`;
  }
  return s;
}
