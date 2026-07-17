// Parse a synthesis markdown file (body + "## Annex: Supporting Facts") into a
// static JSON snapshot suitable for the Docusaurus "examples" pages.
//
// Usage: node parse.mjs <input.md> <output.json>
//
// Output schema:
// {
//   "title": string,            // first H1
//   "body": string,             // markdown body WITHOUT the annex (citations [N] preserved inline)
//   "bodyHtml": string,         // pre-rendered HTML with <sup class="okt-cite" ...> tags
//   "facts": {                  // keyed by citation number as string
//     "1": {
//       "text": string,
//       "sources": [string, ...],
//       "posture"?: "supports" | "contradicts" | "related"  // present when the
//                     // source file tagged this fact's relationship to the
//                     // sentence it cites. Optional; older files have none.
//     },
//     ...
//   },
//   "citationsUsed": number[],  // distinct citation numbers actually appearing in body, sorted
//   "factCount": number
// }
//
// Citation markers
// ----------------
// Two inline marker shapes are accepted, both emitted by the frontend's
// "Copy with cites" feature (frontend/src/lib/citedCopy.js):
//
//   [N]            — no posture (legacy / classifier not configured)
//   [N:supp]       — posture = supports   (short tag)
//   [N:contr]      — posture = contradicts
//   [N:rel]        — posture = related
//
// The short tags are mapped to the full posture words. Multiple markers may
// appear in a row, e.g. `[1:supp][2:contr]`.
//
// In the annex, each fact entry may carry its posture as a parenthesized
// label before the text:
//
//   [N] (supports) <fact text>
//   [N] (contradicts) <fact text>
//   [N] (related) <fact text>
//
// When both the inline marker and the annex declare a posture, the annex
// wins (it is the canonical home for the relationship label). When only the
// inline marker carries one, that posture is used.
//
// The Docusaurus component renders each [N] as a clickable superscript that
// opens a popover showing facts[N]. When a posture is present, the
// superscript and the popover header carry a colored badge so the reader
// sees the relationship (supports / contradicts / related) at a glance.

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

// 2. Title = first H1 in body, with trailing [N] citation markers stripped.
const titleLine = bodyLines.find((l) => /^#\s+/.test(l));
const title = titleLine
  ? titleLine.replace(/^#\s+/, "").replace(/\s*\[\d+\](?:\[\d+\])*\s*$/, "").trim()
  : "Untitled Synthesis";

// 2b. Strip the leading H1 from the body — Docusaurus renders the page title
//     from frontmatter, so keeping the H1 would duplicate it.
const h1Idx = bodyLines.findIndex((l) => /^#\s+/.test(l));
if (h1Idx !== -1) {
  bodyLines.splice(h1Idx, 1);
  if (bodyLines[h1Idx] !== undefined && bodyLines[h1Idx].trim() === "") bodyLines.splice(h1Idx, 1);
  while (bodyLines.length && bodyLines[bodyLines.length - 1].trim() === "") bodyLines.pop();
}
const body = bodyLines.join("\n").trim() + "\n";

// 3. Parse the annex into facts: entries look like
//    [N] <fact text>
//        Sources:
//        - <url>
//        - <url>
//    <blank>
//
//    The "Copy with cites" exporter also emits a parenthesized posture
//    label between the [N] and the text:
//
//    [N] (supports) <fact text>
//    [N] (contradicts) <fact text>
//    [N] (related) <fact text>
//
//    Posture is optional; legacy files omit it. We capture it here so the
//    rendered superscripts and popovers can show the relationship.
const annexLines = lines.slice(annexIdx + 1);
const facts = {};
let i = 0;
// [N] optionally followed by (posture) then the text.
const annexEntryRe = /^\[(\d+)\]\s*(?:\((supports|contradicts|related)\)\s+)?(.*)$/;
while (i < annexLines.length) {
  const m = annexLines[i].match(annexEntryRe);
  if (!m) { i++; continue; }
  const num = m[1];
  const posture = m[2] || null; // null when absent
  const text = m[3].trim();
  i++;
  // Continuation lines until "Sources:" or blank line: fact text may wrap.
  // But in our files each fact is a single line; if a following non-blank,
  // non-"Sources:" line appears, treat it as part of the text.
  while (i < annexLines.length) {
    const l = annexLines[i];
    if (l.trim() === "") break;
    if (/^\s*Sources:\s*$/i.test(l.trim())) break;
    if (annexEntryRe.test(l)) break;
    // wrapped continuation — append
    // (rare; join with space)
    // note: not mutating the captured `text` since it's const
    i++;
  }
  // Now expect "Sources:" then a list of "- <url>" lines.
  const sources = [];
  if (i < annexLines.length && /^\s*Sources:\s*$/i.test(annexLines[i].trim())) {
    i++;
    while (i < annexLines.length) {
      const l = annexLines[i];
      if (l.trim() === "") { i++; break; }
      if (annexEntryRe.test(l)) break;
      const urlM = l.match(/^\s*-\s+(.+?)\s*$/);
      if (urlM) sources.push(urlM[1].trim());
      i++;
    }
  }
  const entry = { text, sources };
  if (posture) entry.posture = posture;
  facts[num] = entry;
  // skip blank(s) between entries
  while (i < annexLines.length && annexLines[i].trim() === "") i++;
}

// 4. Distinct citation numbers actually used in the body, and any inline
//    posture tags. Citations look like [N] or [N:tag] where tag is one of
//    supp / contr / rel (short forms emitted by citedCopy.js). Several
//    markers may appear in a row: [1][2][3] or [1:supp][2:contr].
//    Avoid matching markdown link syntax [text](url) — require N purely
//    numeric AND not followed by "(" (which would make it a markdown link
//    [text](url)).
//
//    Inline posture tags are only used to fill in a fact's posture when
//    the annex didn't already declare one (the annex is canonical).
const citeRe = /\[(\d+)(?::(supp|contr|rel))?\](?!\()/g;
const used = new Set();
const inlinePosture = new Map(); // num (string) -> posture word
const tagToPosture = { supp: "supports", contr: "contradicts", rel: "related" };
let cm;
while ((cm = citeRe.exec(body)) !== null) {
  const n = parseInt(cm[1], 10);
  used.add(n);
  if (cm[2]) {
    const p = tagToPosture[cm[2]];
    if (p && !inlinePosture.has(String(n))) inlinePosture.set(String(n), p);
  }
}
const citationsUsed = Array.from(used).sort((a, b) => a - b);

// Fold inline posture into facts only when the annex didn't set one.
for (const [num, p] of inlinePosture) {
  if (facts[num] && !facts[num].posture) facts[num].posture = p;
}

// 5. Sanity: every used citation should have a fact. Report missing ones.
const missing = citationsUsed.filter((n) => !facts[String(n)]);
if (missing.length) {
  console.error(`WARNING: ${missing.length} citations used in body have no fact entry: ${missing.slice(0, 10).join(", ")}${missing.length > 10 ? " ..." : ""}`);
}
const unused = Object.keys(facts).map((k) => parseInt(k, 10)).filter((n) => !used.has(n));
if (unused.length) {
  console.error(`NOTE: ${unused.length} fact entries are never cited in the body (not an error).`);
}

// 6. Render the body markdown to HTML, converting [N] citations into
//    interactive <sup class="okt-cite" data-n="N">N</sup> tags. We use `marked`
//    (a devDependency of the docs project) to render GFM markdown (tables,
//    strikethrough, lists, etc.). To avoid replacing [N] inside <code>/<pre>
//    blocks, we split the rendered HTML on code spans/blocks and only replace
//    in the non-code segments.
//
// `marked` is resolved relative to the docs project so the parser script can
// be run from anywhere.
import path from "node:path";
import { createRequire } from "node:module";
const require = createRequire(import.meta.url);
// import.meta.url is .../scripts/parse_examples/parse.mjs
// docsRoot is .../docs (three levels up from the script, then into docs)
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

// Replace [N] and [N:tag] (not followed by "(") with sup tags, but NOT
// inside code blocks. Split on <pre>...</pre> and <code>...</code>,
// replace only in odd-indexed (non-code) segments.
//
// The sup tag carries:
//   class="okt-cite"            — base styling + click target
//   class="okt-cite okt-cite--<posture>" — posture-colored variant
//   data-n="N"                  — citation number (for popover lookup)
//   data-posture="<posture>"    — present only when a posture is known
//   data-tag="<short>"          — short inline label ("supp"/"contr"/"rel")
//                                  shown as the visible text when posture
//                                  is present; otherwise just the number
//
// The CSS + component decide how to render the posture (color + label).
const codeSplitRe = /(<pre[\s\S]*?<\/pre>|<code[\s\S]*?<\/code>)/gi;
const parts = rawHtml.split(codeSplitRe);
const citeReplaceRe = /\[(\d+)(?::(supp|contr|rel))?\](?!\()/g;
for (let j = 0; j < parts.length; j++) {
  if (j % 2 === 1) continue; // code block — skip
  parts[j] = parts[j].replace(
    citeReplaceRe,
    (_m, n, tag) => {
      const num = String(n);
      const posture = facts[num]?.posture || (tag ? tagToPosture[tag] : null);
      if (posture) {
        const short = posture === "supports" ? "supp"
          : posture === "contradicts" ? "contr" : "rel";
        return `<sup class="okt-cite okt-cite--${posture}" data-n="${n}" data-posture="${posture}">${n}:${short}</sup>`;
      }
      return `<sup class="okt-cite" data-n="${n}">${n}</sup>`;
    }
  );
}
const bodyHtml = parts.join("");

const out = { title, body, bodyHtml, facts, citationsUsed, factCount: Object.keys(facts).length };
await fs.writeFile(outPath, JSON.stringify(out, null, 0), "utf8"); // 0 = compact, smaller file
console.log(`Wrote ${outPath}: "${title}" — ${citationsUsed.length} citations used, ${out.factCount} facts parsed, ${missing.length} missing.`);