import { segmentSentences } from "./sentences";

// Build a cited copy of a report: the original markdown body with
// the author's inline [text](<fact:uuid>) links replaced with [D{M}]
// direct-cite markers, inline [N] markers after sentences that have
// auto-matched facts (not already covered by a direct cite), and an
// annex listing each fact's full text + source URLs grouped by source.
//
// Direct cites come FIRST in the annex, auto-matched facts second.
// A fact that is BOTH inline-cited and auto-annotated appears ONLY
// in the direct section (the direct cite takes priority — it is the
// author's explicit citation, not an embedding-similarity match).
// When such a deduplicated fact had a posture label from its
// annotation (supports/contradicts/related), the posture is kept and
// displayed in the direct section.
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

  // Posture lookup across ALL annotations (including deduplicated
  // ones), so a direct cite that was also auto-annotated keeps its
  // posture label in the direct section.
  const postureTag = { supports: "supp", contradicts: "contr", related: "rel" };
  const postureByFact = new Map();
  for (const a of anns) {
    if (a.posture && !postureByFact.has(a.fact_id)) {
      postureByFact.set(a.fact_id, a.posture);
    }
  }

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

  // Replace the author's inline [text](<fact:uuid>) / [text](<uuid>)
  // links with [D{M}] markers. Every inline-cited fact gets its link
  // replaced (not just inline-only ones) so the copied text carries
  // clean [D{M}] markers instead of raw UUIDs. Deduplicated facts
  // (inline + auto) keep their posture tag on the [D{M}] marker.
  const UUID = "[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}";
  const directLinkRe = new RegExp(`\\[[^\\]]*\\]\\(<\\s*(?:fact\\s*:\\s*)?(${UUID})\\s*>\\)`, "g");
  const outputBody = bodyMd.replace(directLinkRe, (full, id) => {
    const d = directNumber.get(id);
    if (d != null) {
      const tag = postureTag[postureByFact.get(id)];
      return tag ? `[D${d}:${tag}]` : `[D${d}]`;
    }
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

  // Annex: direct cites FIRST (grouped by source), then auto-matched
  // facts (grouped by source). Grouping by source reduces context
  // consumption by listing each source's URL once with the facts that
  // come from it, instead of repeating the URL per fact.
  if (directNumber.size > 0 || citationNumber.size > 0) {
    output += "\n\n---\n\n## Annex: Supporting Facts\n\n";

    // Direct cites first.
    if (directNumber.size > 0) {
      output +=
        "### Direct cites\n\nThese facts were cited directly by the researcher or AI researcher in the text to support the claim. Each [D{M}] marker in the body links to a fact below.\n\n";
      const sortedDirect = [...directNumber.entries()].sort((a, b) => a[1] - b[1]);
      const directEntries = sortedDirect.map(([fid, num]) => {
        const info = inline.get(fid) || {};
        const posture = postureByFact.get(fid);
        return {
          fid,
          num,
          text: info.text || "",
          sources: info.sources || [],
          error: info.error,
          posture,
        };
      });
      output += groupBySource(directEntries, "D");
    }

    // Auto-matched facts second.
    if (citationNumber.size > 0) {
      output +=
        "### Auto-matched facts\n\nThese facts were retrieved automatically from the knowledge base for additional auditability against the broad corpus. Each [N] marker in the body links to a fact below.\n\n";
      const factTextById = new Map();
      for (const a of autoAnns) {
        if (!factTextById.has(a.fact_id)) factTextById.set(a.fact_id, a.text || "");
      }
      const sortedAuto = [...citationNumber.entries()].sort((a, b) => a[1] - b[1]);
      const autoEntries = sortedAuto.map(([fid, num]) => ({
        fid,
        num,
        text: factTextById.get(fid) || "",
        sources: factSources?.get(fid) || [],
        error: null,
        posture: postureByFact.get(fid),
      }));
      output += groupBySource(autoEntries, "");
    }
  }

  return output;
}

// groupBySource groups fact entries by their source URL (or
// parsed_title when available) so each source is listed once with
// all its facts under it, reducing context consumption. Facts with no
// sources are grouped under "(unavailable)". The `prefix` controls
// the marker form: "D" for direct cites ([D1]), "" for auto ([1]).
function groupBySource(entries, prefix) {
  // Build a source-key -> { label, url, entries } map, preserving
  // first-seen order of sources.
  const sourceOrder = [];
  const sourceMap = new Map();
  const noSource = { label: "(unavailable)", url: null, entries: [] };
  for (const e of entries) {
    if (!e.sources || e.sources.length === 0) {
      if (e.error) {
        noSource.entries.push({ ...e, errorSources: true });
      } else {
        noSource.entries.push(e);
      }
      continue;
    }
    for (const src of e.sources) {
      const key = src.url || src.parsed_title || "(untitled)";
      let bucket = sourceMap.get(key);
      if (!bucket) {
        bucket = { label: src.parsed_title || src.url || key, url: src.url, entries: [] };
        sourceMap.set(key, bucket);
        sourceOrder.push(key);
      }
      bucket.entries.push(e);
    }
  }

  let out = "";
  for (const key of sourceOrder) {
    const bucket = sourceMap.get(key);
    out += `**${bucket.label}**\n`;
    if (bucket.url) out += `${bucket.url}\n\n`;
    // Dedup facts within a source bucket (a fact with multiple sources
    // appears under each, but not twice under the same source).
    const seen = new Set();
    for (const e of bucket.entries) {
      if (seen.has(e.fid)) continue;
      seen.add(e.fid);
      const marker = prefix ? `[${prefix}${e.num}]` : `[${e.num}]`;
      const postureLabel = e.posture ? ` (${e.posture})` : "";
      out += `- ${marker}${postureLabel} ${e.text}\n`;
    }
    out += "\n";
  }
  // Facts with no sources (or fetch errors).
  if (noSource.entries.length > 0) {
    const seen = new Set();
    out += "**(unavailable)**\n\n";
    for (const e of noSource.entries) {
      if (seen.has(e.fid)) continue;
      seen.add(e.fid);
      const marker = prefix ? `[${prefix}${e.num}]` : `[${e.num}]`;
      const postureLabel = e.posture ? ` (${e.posture})` : "";
      if (e.error) {
        out += `- ${marker}(unavailable: ${e.error}) ${e.text}\n`;
      } else {
        out += `- ${marker}${postureLabel} ${e.text}\n`;
      }
    }
    out += "\n";
  }
  return out;
}
