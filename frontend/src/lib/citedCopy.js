import { segmentSentences } from "./sentences";

// Build a cited copy of a report: the original markdown body with
// the author's inline [text](<fact:uuid>) links replaced with [D{M}]
// direct-cite markers, inline [N] markers after sentences that have
// auto-matched facts (not already covered by a direct cite), and an
// annex listing each fact's full text + source URLs.
//
// Direct cites come FIRST in the annex, auto-matched facts second.
// A fact that is BOTH inline-cited and auto-annotated appears ONLY
// in the direct section (the direct cite takes priority — it is the
// author's explicit citation, not an embedding-similarity match).
//
// Args:
//   - bodyMd:       raw markdown body
//   - annotations:  report annotation rows (each has sentence_index,
//                   fact_id, text, score, posture)
//   - factSources:  Map<fact_id, source[]> for auto-annotated facts.
//   - inlineFacts:  Map<fact_id, {text, sources, error}> for facts the
//                   author cited inline. `error` is set when the fetch
//                   failed (404, network); the annex records it so the
//                   reader sees the citation was attempted.
//
// Posture (supports/contradicts/related) is surfaced as a short inline
// tag ([1:supp], [2:contr], [3:rel]). Direct cites carry [D{M}].
export function buildCitedText(bodyMd, annotations, factSources, inlineFacts) {
  if (!bodyMd) return "";

  const anns = annotations || [];
  const inline = inlineFacts instanceof Map ? inlineFacts : new Map();

  // The set of fact_ids the author cited inline. These take priority
  // over auto-annotations: they go in the direct section and are
  // removed from the auto-annotated section (no duplication).
  const inlineIds = new Set(inline.keys());

  // Auto-annotated facts NOT already covered by a direct cite. These
  // get [N] markers and go in the second annex section.
  const autoAnns = anns.filter((a) => !inlineIds.has(a.fact_id));

  // Group auto-annotated fact_ids by sentence_index, preserving
  // first-seen order.
  const factsBySentence = new Map();
  for (const a of autoAnns) {
    const arr = factsBySentence.get(a.sentence_index) || [];
    if (!arr.includes(a.fact_id)) arr.push(a.fact_id);
    factsBySentence.set(a.sentence_index, arr);
  }

  // Assign [N] numbers to auto-only facts in sentence order.
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

  // Assign [D{M}] numbers to ALL inline-cited facts, in first-
  // appearance order (the inlineFacts map preserves insertion order).
  const directNumber = new Map();
  let nextDirect = 1;
  for (const fid of inline.keys()) {
    if (!directNumber.has(fid)) directNumber.set(fid, nextDirect++);
  }

  // Map each auto-annotation's posture to a short inline tag.
  const postureTag = { supports: "supp", contradicts: "contr", related: "rel" };
  const postureByFact = new Map();
  for (const a of autoAnns) {
    if (a.posture && !postureByFact.has(a.fact_id)) {
      postureByFact.set(a.fact_id, a.posture);
    }
  }

  // Replace the author's inline [text](<fact:uuid>) / [text](<uuid>)
  // links with [D{M}] markers. Every inline-cited fact gets its link
  // replaced (not just inline-only ones) so the copied text carries
  // clean [D{M}] markers instead of raw UUIDs.
  const UUID = "[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}";
  const directLinkRe = new RegExp(`\\[[^\\]]*\\]\\(<\\s*(?:fact\\s*:\\s*)?(${UUID})\\s*>\\)`, "g");
  const outputBody = bodyMd.replace(directLinkRe, (full, id) => {
    const d = directNumber.get(id);
    if (d != null) return `[D${d}]`;
    return full;
  });

  // Insert auto-citation [N] markers after each auto-cited sentence's
  // terminal punctuation. Re-segment the link-replaced body so the
  // offsets line up.
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

  // Annex: direct cites FIRST, then auto-matched facts.
  if (directNumber.size > 0 || citationNumber.size > 0) {
    output += "\n\n---\n\n## Annex: Supporting Facts\n\n";

    // Direct cites first.
    if (directNumber.size > 0) {
      output += "### Direct cites (author's inline citations)\n\n";
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

    // Auto-matched facts second.
    if (citationNumber.size > 0) {
      output += "### Auto-matched facts\n\n";
      const factTextById = new Map();
      for (const a of autoAnns) {
        if (!factTextById.has(a.fact_id)) factTextById.set(a.fact_id, a.text || "");
      }
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
