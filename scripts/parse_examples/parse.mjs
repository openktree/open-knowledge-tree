// Parse a synthesis markdown file (body + "## Annex: Supporting Facts") into a
// static JSON snapshot suitable for the Docusaurus "examples" pages.
//
// Usage: node parse.mjs <input.md> <output.json>
//
// Output schema:
// {
//   "title": string,            // first H1
//   "body": string,             // markdown body WITHOUT the annex (citations [N]/[DN] preserved inline)
//   "bodyHtml": string,         // pre-rendered HTML with <sup class="okt-cite" ...> tags
//   "facts": {                  // auto-matched facts (keyed by citation number as string)
//     "1": {
//       "text": string,
//       "sources": [string, ...],
//       "posture"?: "supports" | "contradicts" | "related"
//     },
//     ...
//   },
//   "directCites": {            // direct cites (keyed by the digit after "D", as string)
//     "1": { text, sources, posture? } | { "unavailable": true },
//     ...
//   },
//   "citationsUsed": number[],  // distinct auto-matched [N] citation numbers actually used in body
//   "directCitesUsed": number[], // distinct [DN] direct-cite numbers actually used in body
//   "factCount": number,
//   "directCiteCount": number
// }
//
// Citation markers
// ----------------
// Three classes of inline marker are recognized:
//
//   [N]            — auto-matched annotation (no posture)
//   [N:supp]       — auto-matched annotation with posture = supports (short tag)
//   [N:contr]      — auto-matched annotation with posture = contradicts
//   [N:rel]        — auto-matched annotation with posture = related
//
//   [D{M}]         — direct cite (no posture). The "D" prefix marks it as a
//                    fact the researcher/AI-researcher explicitly cited from
//                    a source, as opposed to one the system auto-matched.
//   [D{M}:supp]    — direct cite with posture = supports
//   [D{M}:contr]   — direct cite with posture = contradicts
//   [D{M}:rel]     — direct cite with posture = related
//
// The short tags are mapped to the full posture words. Multiple markers may
// appear in a row, e.g. `[1:supp][2:contr]` or `([D2:supp], [D1:supp])`.
//
// Annex layout
// ------------
// The annex is split into two optional subsections, identified by H3 headings:
//
//   ## Annex: Supporting Facts
//
//   ### Direct cites            ← facts for [D{M}] markers (optional)
//   ...
//
//   ### Auto-matched facts      ← facts for [N] markers (optional)
//   ...
//
// Either section may be absent; the parser carries on with whatever is found.
// If neither H3 is present, the entire annex is treated as one auto-matched
// section (legacy compatibility).
//
// Within a section, two source-listing shapes are supported:
//
//   (New format — grouped by source.) The source is announced by a bold title
//   line followed by a URL line; all entries below share that source until a
//   new source heading appears:
//
//     **Source title here**
//     https://example.com/source
//
//     - [D1] (supports) Fact text 1.
//     - [D2] (related) Fact text 2.
//
//     **Next source**
//     https://...
//
//   (Legacy format — per-entry "Sources:" block.) Each entry carries its own
//   indented "Sources:" block:
//
//     [1] Fact text.
//         Sources:
//         - https://example.com/a
//
// Both shapes may coexist in the same section. When an entry has its own
// "Sources:" block, those URLs are merged with the section's current source.
//
// Unavailable entries
// -------------------
// Some annexes list a missing fact as a markdown-style link:
//
//   - [D63](unavailable: fact not found)
//
// These are parsed into a placeholder entry `{ unavailable: true }` so the
// popover can show a graceful "fact unavailable" message instead of opening
// an empty panel. The body's [D63] citation still resolves to that entry.
//
// Concept / fact link pre-processing
// ----------------------------------
// The body may contain markdown links of the form
//   [Project MKUltra](<concept:03330290-...>)
//   [Chaos: ...](<concept:e603f54d-...>)
// which are fine for the live app (where concept:<uuid> resolves to a route)
// but break in a static snapshot — the browser would try to navigate to a
// `concept:` URL. We pre-process the body to convert these into bold text
// (preserving the visible label and signalling that it is a concept
// reference) before handing the body to the markdown renderer. Standalone
// `[fact:<uuid>]` / `[concept:<uuid>]` text references are dropped (they
// have no useful static representation).

import fs from "node:fs/promises";

const [, , inPath, outPath] = process.argv;
if (!inPath || !outPath) {
  console.error("Usage: node parse.mjs <input.md> <output.json>");
  process.exit(1);
}

const raw = await fs.readFile(inPath, "utf8");
const lines = raw.split(/\r?\n/);

// 1. Find the annex boundary: the first "## Annex: Supporting Facts" heading.
const annexIdx = lines.findIndex((l) => /^##\s+Annex:\s*Supporting Facts\s*$/i.test(l.trim()));
if (annexIdx === -1) {
  throw new Error(`No "## Annex: Supporting Facts" heading found in ${inPath}`);
}

// Body = everything before the annex heading (strip trailing blank lines / hr).
let bodyLines = lines.slice(0, annexIdx);
// Trim a trailing "---" hr and surrounding blanks that often precede the annex.
while (bodyLines.length && bodyLines[bodyLines.length - 1].trim() === "") bodyLines.pop();
if (bodyLines.length && bodyLines[bodyLines.length - 1].trim() === "---") bodyLines.pop();
while (bodyLines.length && bodyLines[bodyLines.length - 1].trim() === "") bodyLines.pop();

// 2. Title = first H1 in body, with trailing [N] / [N:tag] / [DN] / [DN:tag]
//    citation markers stripped.
const titleLine = bodyLines.find((l) => /^#\s+/.test(l));
const title = titleLine
  ? titleLine.replace(/^#\s+/, "").replace(/\s*(?:\[(?:D?\d+)(?::[a-z]+)?\]\s*)+$/, "").trim()
  : "Untitled Synthesis";

// 2b. Strip the leading H1 from the body — Docusaurus renders the page title
//     from frontmatter, so keeping the H1 would duplicate it.
const h1Idx = bodyLines.findIndex((l) => /^#\s+/.test(l));
if (h1Idx !== -1) {
  bodyLines.splice(h1Idx, 1);
  if (bodyLines[h1Idx] !== undefined && bodyLines[h1Idx].trim() === "") bodyLines.splice(h1Idx, 1);
  while (bodyLines.length && bodyLines[bodyLines.length - 1].trim() === "") bodyLines.pop();
}

// Pre-process body: convert [text](<concept:UUID>) / [text](<fact:UUID>)
// markdown links into bold text (preserving the visible label), and drop
// standalone [concept:UUID] / [fact:UUID] text references.
//
// The concept-link regex matches `[label](<concept:uuid>)` where the URL is
// wrapped in angle brackets. `marked` would otherwise render these as <a href="concept:...">
// which is broken in a static page.
const conceptLinkRe = /\[([^\]]+)\]\(<(?:concept|fact):[0-9a-fA-F-]+\>\)/g;
const standaloneRefRe = /\[(?:concept|fact):[0-9a-fA-F-]+\]/g;
bodyLines = bodyLines.map((l) =>
  l
    .replace(conceptLinkRe, "**$1**")
    .replace(standaloneRefRe, "")
);
const body = bodyLines.join("\n").trim() + "\n";

// 3. Parse the annex into two fact maps: `directCites` (for [D{M}]) and
//    `facts` (for [N]). The annex may be split by `### Direct cites` and
//    `### Auto-matched facts` H3 headings; if neither heading is present
//    (legacy files), everything is treated as auto-matched.
const annexLines = lines.slice(annexIdx + 1);

// Split the annex lines into sections by H3 heading. Each section carries
// its `kind` ("direct" | "auto") and the lines belonging to it.
const sections = [];
let curKind = "auto"; // default for legacy files with no H3
let curLines = [];
let sawAnyH3 = false;
for (const l of annexLines) {
  const h3m = l.match(/^###\s+(.+?)\s*$/);
  if (h3m) {
    if (sawAnyH3 || curLines.length) {
      sections.push({ kind: curKind, lines: curLines });
    }
    sawAnyH3 = true;
    const name = h3m[1].toLowerCase();
    curKind = name.includes("direct") ? "direct" : "auto";
    curLines = [];
    continue;
  }
  curLines.push(l);
}
if (curLines.length) sections.push({ kind: curKind, lines: curLines });

const directCites = {};
const facts = {};
for (const sec of sections) {
  const target = sec.kind === "direct" ? directCites : facts;
  parseAnnexSection(sec.lines, target);
}

/**
 * Parse one annex section's lines into the given target map.
 *
 * Each entry's key is the bare number as a string — for direct cites that
 * means the digit part of "D{N}" (e.g. "1" for "[D1]"); for auto-matched
 * facts it's just the number (e.g. "1" for "[1]").
 *
 * Supports two source-listing shapes (see header comment) and the
 * `(unavailable: ...)` placeholder entries.
 */
function parseAnnexSection(secLines, target) {
  let currentSources = []; // URLs from the most recent source heading
  let i = 0;
  // Entry regex. Captures:
  //   group 1: the citation key — "D1" or "1" (with optional leading "- ")
  //   group 2: posture word (supports|contradicts|related) — optional
  //   group 3: remaining text on the entry line
  const entryRe =
    /^(?:-\s+)?\[(D?\d+)\]\s*(?:\((supports|contradicts|related)\)\s+)?(.*)$/;
  // Unavailable placeholder: `- [D63](unavailable: fact not found)`
  const unavailableRe = /^(?:-\s+)?\[(D?\d+)\]\((unavailable:[^)]*)\)\s*$/;
  // Bold source-title line: `**Source title**`
  const sourceHeadingRe = /^\*\*([^*]+)\*\*\s*$/;
  // Bare URL line.
  const urlRe = /^\s*(https?:\/\/\S+)\s*$/i;

  while (i < secLines.length) {
    const l = secLines[i];
    const trimmed = l.trim();

    // Blank line — advance.
    if (trimmed === "") { i++; continue; }

    // Bold source title: start a new source group.
    const sh = trimmed.match(sourceHeadingRe);
    if (sh) {
      // If the title is "(unavailable)", reset sources to empty (the
      // entries below are placeholders). Otherwise, look ahead for a URL
      // on the next non-blank line.
      const titleText = sh[1].trim();
      currentSources = [];
      // Peek at the next non-blank line for a URL. If found, consume it;
      // if not, leave i pointing at the line after the title so the next
      // iteration re-examines it (it might be the first entry of the
      // group, e.g. an "(unavailable)" placeholder group).
      let j = i + 1;
      while (j < secLines.length && secLines[j].trim() === "") j++;
      if (j < secLines.length && urlRe.test(secLines[j].trim())) {
        currentSources.push(secLines[j].trim().match(urlRe)[1]);
        i = j + 1;
      } else {
        i = i + 1;
      }
      continue;
    }

    // Bare URL line — append to current source group.
    const ul = trimmed.match(urlRe);
    if (ul) {
      // Avoid duplicating the URL we already consumed as the heading's URL.
      if (!currentSources.includes(ul[1])) currentSources.push(ul[1]);
      i++;
      continue;
    }

    // Unavailable placeholder entry.
    const un = trimmed.match(unavailableRe);
    if (un) {
      const key = stripD(un[1]);
      target[key] = { unavailable: true };
      i++;
      // Skip any trailing "Sources:" block (unlikely, but be safe).
      continue;
    }

    // Normal entry: `[N] (posture) text`  or  `- [D{N}] (posture) text`
    const m = trimmed.match(entryRe);
    if (m) {
      const rawKey = m[1];
      const key = stripD(rawKey);
      const posture = m[2] || null;
      let text = m[3].trim();
      i++;
      // Continuation lines until: blank line, next entry, source heading,
      // or a bare URL. Wrap into the fact text.
      while (i < secLines.length) {
        const nl = secLines[i];
        const nlt = nl.trim();
        if (nlt === "") break;
        if (entryRe.test(nlt) || unavailableRe.test(nlt)) break;
        if (sourceHeadingRe.test(nlt)) break;
        if (urlRe.test(nlt)) break;
        if (/^Sources:\s*$/i.test(nlt)) break;
        text += " " + nlt;
        i++;
      }
      // Optional legacy "Sources:" block.
      const sources = [...currentSources];
      if (i < secLines.length && /^\s*Sources:\s*$/i.test(secLines[i].trim())) {
        i++;
        while (i < secLines.length) {
          const nl = secLines[i];
          const nlt = nl.trim();
          if (nlt === "") { i++; break; }
          if (entryRe.test(nlt) || unavailableRe.test(nlt)) break;
          if (sourceHeadingRe.test(nlt) || urlRe.test(nlt)) break;
          const urlM = nlt.match(/^\s*-\s+(.+?)\s*$/);
          if (urlM && !sources.includes(urlM[1])) sources.push(urlM[1].trim());
          i++;
        }
      }
      const entry = { text, sources };
      if (posture) entry.posture = posture;
      target[key] = entry;
      continue;
    }

    // Anything else (descriptive prose in the annex) — skip.
    i++;
  }
}

/** Strip the leading "D" from a direct-cite key, or return as-is for [N]. */
function stripD(key) {
  return key.startsWith("D") ? key.slice(1) : key;
}

// 4. Distinct citation numbers actually used in the body. [N] / [N:tag] go into
//    citationsUsed; [D{M}] / [D{M}:tag] go into directCitesUsed. We avoid
//    matching markdown link syntax [text](url) — require the marker not be
//    followed by "(".
const citeRe = /\[(D?\d+)(?::(supp|contr|rel))?\](?!\()/g;
const usedAuto = new Set();
const usedDirect = new Set();
const inlinePosture = new Map(); // key -> posture word (only for auto-matched)
const tagToPosture = { supp: "supports", contr: "contradicts", rel: "related" };
let cm;
while ((cm = citeRe.exec(body)) !== null) {
  const raw = cm[1];
  const isDirect = raw.startsWith("D");
  const n = parseInt(isDirect ? raw.slice(1) : raw, 10);
  if (isDirect) usedDirect.add(n);
  else usedAuto.add(n);
  if (cm[2]) {
    const p = tagToPosture[cm[2]];
    if (p && !inlinePosture.has(String(n))) inlinePosture.set(String(n), p);
  }
}
const citationsUsed = Array.from(usedAuto).sort((a, b) => a - b);
const directCitesUsed = Array.from(usedDirect).sort((a, b) => a - b);

// Fold inline posture into auto-matched facts only when the annex didn't
// already set one. (Direct cites' postures come from the annex canonical.)
for (const [num, p] of inlinePosture) {
  if (facts[num] && !facts[num].posture) facts[num].posture = p;
}

// 5. Sanity: report missing/unreferenced.
const missingAuto = citationsUsed.filter((n) => !facts[String(n)]);
if (missingAuto.length) {
  console.error(`WARNING: ${missingAuto.length} auto-matched citations used in body have no fact entry: ${missingAuto.slice(0, 10).join(", ")}${missingAuto.length > 10 ? " ..." : ""}`);
}
const missingDirect = directCitesUsed.filter((n) => !directCites[String(n)]);
if (missingDirect.length) {
  console.error(`WARNING: ${missingDirect.length} direct-cite markers used in body have no fact entry: ${missingDirect.slice(0, 10).join(", ")}${missingDirect.length > 10 ? " ..." : ""}`);
}
const unusedAuto = Object.keys(facts).map((k) => parseInt(k, 10)).filter((n) => !usedAuto.has(n));
if (unusedAuto.length) {
  console.error(`NOTE: ${unusedAuto.length} auto-matched fact entries are never cited in the body (not an error).`);
}
const unusedDirect = Object.keys(directCites).map((k) => parseInt(k, 10)).filter((n) => !usedDirect.has(n));
if (unusedDirect.length) {
  console.error(`NOTE: ${unusedDirect.length} direct-cite entries are never cited in the body (not an error).`);
}

// 6. Render the body markdown to HTML, converting [N] and [D{M}] citations
//    into interactive <sup class="okt-cite" ...> tags. We use `marked` to
//    render GFM markdown (tables, strikethrough, lists, etc.). To avoid
//    replacing [N] inside <code>/<pre> blocks, we split the rendered HTML on
//    code spans/blocks and only replace in the non-code segments.
import path from "node:path";
import { createRequire } from "node:module";
const require = createRequire(import.meta.url);
const scriptDir = path.dirname(new URL(import.meta.url).pathname);
const docsRoot = path.resolve(scriptDir, "../../docs");
let marked;
try {
  marked = require(path.join(docsRoot, "node_modules/marked")).marked;
} catch {
  marked = require("marked").marked;
}
marked.setOptions({ gfm: true, breaks: false });
let rawHtml = marked.parse(body);

// Replace [N] / [N:tag] / [D{M}] / [D{M}:tag] (not followed by "(") with sup
// tags, but NOT inside code blocks. Split on <pre>...</pre> and <code>...</code>,
// replace only in odd-indexed (non-code) segments.
//
// The sup tag carries:
//   class="okt-cite"                       — base styling + click target
//   class="okt-cite okt-cite--<posture>"   — posture-colored variant
//   class="okt-cite okt-cite--direct"       — direct-cite variant (added on
//                                             direct cites so the UI can
//                                             differentiate researcher-chosen
//                                             cites from auto-matched facts)
//   data-n="N"                             — citation number (for popover lookup)
//   data-kind="direct" | "auto"            — which map to look up
//   data-posture="<posture>"               — present only when a posture is known
//   data-tag="<short>"                     — short inline label ("supp"/"contr"/"rel")
//                                            shown as visible text when posture
//                                            is present; otherwise just the number
//
// Visible text:
//   - Direct cite with posture:  "D2:supp"
//   - Direct cite without posture: "D2"
//   - Auto-matched with posture: "2:supp"
//   - Auto-matched without posture: "2"
const codeSplitRe = /(<pre[\s\S]*?<\/pre>|<code[\s\S]*?<\/code>)/gi;
const parts = rawHtml.split(codeSplitRe);
const citeReplaceRe = /\[(D?\d+)(?::(supp|contr|rel))?\](?!\()/g;
for (let j = 0; j < parts.length; j++) {
  if (j % 2 === 1) continue; // code block — skip
  parts[j] = parts[j].replace(
    citeReplaceRe,
    (_m, raw, tag) => {
      const isDirect = raw.startsWith("D");
      const n = isDirect ? raw.slice(1) : raw;
      const num = String(n);
      const kind = isDirect ? "direct" : "auto";
      const entry = isDirect ? directCites[num] : facts[num];
      const posture = entry?.posture || (tag ? tagToPosture[tag] : null);
      const cls = ["okt-cite", `okt-cite--${kind}`];
      if (posture) cls.push(`okt-cite--${posture}`);
      const visiblePrefix = isDirect ? "D" : "";
      if (posture) {
        const short = posture === "supports" ? "supp"
          : posture === "contradicts" ? "contr" : "rel";
        return `<sup class="${cls.join(" ")}" data-n="${n}" data-kind="${kind}" data-posture="${posture}">${visiblePrefix}${n}:${short}</sup>`;
      }
      return `<sup class="${cls.join(" ")}" data-n="${n}" data-kind="${kind}">${visiblePrefix}${n}</sup>`;
    }
  );
}
const bodyHtml = parts.join("");

const out = {
  title,
  body,
  bodyHtml,
  facts,
  directCites,
  citationsUsed,
  directCitesUsed,
  factCount: Object.keys(facts).length,
  directCiteCount: Object.keys(directCites).length,
};
await fs.writeFile(outPath, JSON.stringify(out, null, 0), "utf8"); // 0 = compact, smaller file
console.log(`Wrote ${outPath}: "${title}" — ${citationsUsed.length} auto citations + ${directCitesUsed.length} direct cites used; ${out.factCount} facts + ${out.directCiteCount} direct cites parsed.`);
