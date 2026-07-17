// Parse a synthesis markdown file (body + "## Annex: Supporting Facts") into a
// static JSON snapshot suitable for the Docusaurus "examples" pages.
//
// Usage: node parse.mjs <input.md> <output.json>
//
// Output schema:
// {
//   "title": string,            // first H1
//   "body": string,             // markdown body WITHOUT the annex (citations [N] preserved inline)
//   "facts": {                  // keyed by citation number as string
//     "1": { "text": string, "sources": [string, ...] },
//     ...
//   },
//   "citationsUsed": number[]    // distinct citation numbers actually appearing in body, sorted
// }
//
// The body keeps the original [N] markers. The Docusaurus component is
// responsible for rendering each [N] as a clickable superscript that opens a
// popover showing facts[N].

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
const annexLines = lines.slice(annexIdx + 1);
const facts = {};
let i = 0;
const annexEntryRe = /^\[(\d+)\]\s+(.*)$/;
while (i < annexLines.length) {
  const m = annexLines[i].match(annexEntryRe);
  if (!m) { i++; continue; }
  const num = m[1];
  const text = m[2].trim();
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
  facts[num] = { text, sources };
  // skip blank(s) between entries
  while (i < annexLines.length && annexLines[i].trim() === "") i++;
}

// 4. Distinct citation numbers actually used in the body.
//    Citations look like [N] (possibly several in a row: [1][2][3]).
//    Avoid matching markdown link syntax [text](url) — require N purely numeric
//    AND not followed by "(" (which would make it a markdown link [text](url)).
const citeRe = /\[(\d+)\](?!\()/g;
const used = new Set();
let cm;
while ((cm = citeRe.exec(body)) !== null) {
  used.add(parseInt(cm[1], 10));
}
const citationsUsed = Array.from(used).sort((a, b) => a - b);

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

// Replace [N] (not followed by "(") with sup tags, but NOT inside code blocks.
// Split on <pre>...</pre> and <code>...</code>, replace only in odd-indexed
// (non-code) segments.
const codeSplitRe = /(<pre[\s\S]*?<\/pre>|<code[\s\S]*?<\/code>)/gi;
const parts = rawHtml.split(codeSplitRe);
const citeReplaceRe = /\[(\d+)\](?!\()/g;
for (let j = 0; j < parts.length; j++) {
  if (j % 2 === 1) continue; // code block — skip
  parts[j] = parts[j].replace(
    citeReplaceRe,
    (_m, n) => `<sup class="okt-cite" data-n="${n}">${n}</sup>`
  );
}
const bodyHtml = parts.join("");

const out = { title, body, bodyHtml, facts, citationsUsed, factCount: Object.keys(facts).length };
await fs.writeFile(outPath, JSON.stringify(out, null, 0), "utf8"); // 0 = compact, smaller file
console.log(`Wrote ${outPath}: "${title}" — ${citationsUsed.length} citations used, ${out.factCount} facts parsed, ${missing.length} missing.`);