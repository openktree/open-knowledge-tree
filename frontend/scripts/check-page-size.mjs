#!/usr/bin/env node
// Enforces the "Frontend Page Size Policy" from AGENTS.md.
//
// Run via:  npm run check:pages        (from frontend/)
// or:       just check-pages
//
// Exits 0 when every flat page in src/pages/ is small enough.
// Exits 1 with a report of every violation otherwise.
//
// Thresholds (mirror AGENTS.md exactly — keep in sync):
//   - Flat page file > 150 lines
//   - Flat page file > 100 lines AND defines its own internal subcomponents
//     (functions returning JSX in the same file) — strong signal it should be a folder
//   - Flat page file importing 6+ distinct items from ../components/
//
// We don't count reactive primitives (createSignal / createResource / createMemo):
// a single form legitimately has 4-5 of them and that's fine. The pain signal is
// breadth of UI composition, not local state count.
//
// Escape hatch: a top-of-file comment `// @okt-page-allow-large` plus a justification line
// skips the check for that file. Use sparingly; expect a review comment if you do.

import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative, sep } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = fileURLToPath(new URL(".", import.meta.url));
// frontend/scripts/ -> frontend/
const FRONTEND_ROOT = join(HERE, "..");
const PAGES_DIR = join(FRONTEND_ROOT, "src", "pages");

const LIMITS = {
  maxLines: 150,
  maxLinesWithSubcomponents: 100,
  maxComponentImports: 6,
};

// Matches top-level `function Foo(...)` or `const Foo = ...` inside the page file —
// i.e. internal subcomponents defined alongside the default export. Excludes the
// default export itself ("Sources", "Users", "Login", ...).
const INTERNAL_COMPONENT_RE =
  /^(?:export\s+default\s+function\s+([A-Z]\w+)|function\s+([A-Z]\w+)\s*\(|const\s+([A-Z]\w+)\s*=\s*(?:\([^)]*\)|[A-Z]\w*)\s*=>)/gm;

const COMPONENT_IMPORT_RE =
  /from\s+["'][^"']*components\/([A-Z][A-Za-z0-9_]*)\b[^"']*["']/g;

const ALLOW_DIRECTIVE = "@okt-page-allow-large";

const violations = [];

function* walkPages(dir) {
  for (const name of readdirSync(dir)) {
    const full = join(dir, name);
    const st = statSync(full);
    if (st.isDirectory()) {
      // A page folder is allowed to contain many files; only inspect .jsx/.js at the
      // folder root (index.jsx is the route entry).
      for (const sub of readdirSync(full)) {
        if (sub === "index.jsx" || sub === "index.js") {
          yield join(full, sub);
        }
      }
      continue;
    }
    if (name.endsWith(".jsx") || name.endsWith(".js")) {
      yield full;
    }
  }
}

function checkFile(file) {
  const src = readFileSync(file, "utf8");
  const lines = src.split("\n");
  const rel = relative(FRONTEND_ROOT, file);

  // Top-of-file allow directive? Skip the check, but require a justification on the same line.
  const head = lines.slice(0, 5).join("\n");
  if (head.includes(ALLOW_DIRECTIVE)) {
    // Acceptable forms:   // @okt-page-allow-large: reason
    //                     // @okt-page-allow-large - reason
    //                     // @okt-page-allow-large — reason
    const JUSTIFICATION = new RegExp(
      `${ALLOW_DIRECTIVE}\\s*[:\\-—]\\s*\\S`,
    );
    if (!JUSTIFICATION.test(head)) {
      violations.push({
        file: rel,
        kind: "allow-directive-without-justification",
        detail: `Found '${ALLOW_DIRECTIVE}' but no justification on the same line. ` +
                `Use:  // ${ALLOW_DIRECTIVE}: <one-line reason>`,
      });
    }
    return;
  }

  const issues = [];

  // Count internal subcomponents (excluding the default export).
  const internalComponents = new Set();
  let im;
  while ((im = INTERNAL_COMPONENT_RE.exec(src)) !== null) {
    const name = im[1] || im[2] || im[3];
    if (name) internalComponents.add(name);
  }

  if (lines.length > LIMITS.maxLines) {
    issues.push(
      `${lines.length} lines > limit of ${LIMITS.maxLines}`,
    );
  } else if (
    internalComponents.size > 0 &&
    lines.length > LIMITS.maxLinesWithSubcomponents
  ) {
    issues.push(
      `${lines.length} lines with ${internalComponents.size} internal subcomponents ` +
      `(${[...internalComponents].sort().join(", ")}); > limit of ${LIMITS.maxLinesWithSubcomponents} ` +
      `lines when the page defines its own subcomponents — split them into the page folder`,
    );
  }

  const componentImports = new Set();
  let m;
  while ((m = COMPONENT_IMPORT_RE.exec(src)) !== null) {
    componentImports.add(m[1]);
  }
  if (componentImports.size >= LIMITS.maxComponentImports) {
    issues.push(
      `${componentImports.size} component imports (${[...componentImports].sort().join(", ")}) ` +
      `>= limit of ${LIMITS.maxComponentImports}`,
    );
  }

  if (issues.length > 0) {
    violations.push({
      file: rel.split(sep).join("/"),
      kind: "page-too-large",
      detail: issues.join("; "),
    });
  }
}

for (const file of walkPages(PAGES_DIR)) {
  checkFile(file);
}

if (violations.length === 0) {
  console.log("OK: every page in src/pages/ is within the size budget.");
  process.exit(0);
}

console.error("Frontend page size policy violations:\n");
for (const v of violations) {
  console.error(`  ${v.file}`);
  console.error(`    [${v.kind}] ${v.detail}\n`);
}
console.error(
  `Fix: convert the page to a folder per AGENTS.md "Page folder convention" ` +
  `(${LIMITS.maxLines}-line limit, subcomponent split, state in index.jsx).`,
);
console.error(
  `Or, in exceptional cases, add a one-line justification at the top of the file:\n` +
  `  // ${ALLOW_DIRECTIVE}: <reason this page is allowed to stay flat>`,
);
process.exit(1);
