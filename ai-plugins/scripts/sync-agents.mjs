#!/usr/bin/env node
// scripts/sync-agents.mjs
//
// Single source of truth: the five agent markdown files at .opencode/agent/*.md
// (opencode format, with `description`, `mode`, and optional `model` frontmatter).
//
// This generator emits, for each source agent:
//   - ai-plugins/agents/<name>.md      Claude Code + VS Code/Copilot format
//                                      (YAML frontmatter: name, description, model, tools)
//   - ai-plugins/agents/<name>.toml   OpenAI Codex CLI format
//                                      (name, description, developer_instructions, model, sandbox_mode)
//   - ai-plugins/opencode/agents/<name>.md   opencode-format copy (verbatim body, normalized frontmatter)
//
// Generated files are checked into git so non-Node users can read them. The
// `--check` flag (or `just check-plugins`) regenerates and exits non-zero if
// anything drifted, which is the CI gate.
//
// Run:   node scripts/sync-agents.mjs          # regenerate
//        node scripts/sync-agents.mjs --check  # fail on drift
//
// The emitter functions (parseFrontmatter, serializeFrontmatter, emitClaudeMd,
// emitCodexToml, emitOpencodeMd) are exported for unit testing.

import { readFile, writeFile, mkdir, readdir, rm } from "node:fs/promises";
import { existsSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = dirname(fileURLToPath(import.meta.url));
const ROOT = resolve(__dirname, "..", "..");
const SRC_DIR = join(ROOT, ".opencode", "agent");
const OUT_AGENTS = join(__dirname, "..", "agents");
const OUT_OPENCODE = join(__dirname, "..", "opencode", "agents");

// MCP server name used by the OKT agents. All OKT MCP tools are namespaced
// under this server. Claude Code's tool format is `mcp__<server>__<tool>` and
// the `mcp__okt__*` glob grants all of them. VS Code/Copilot uses
// `<server>/*` (we emit `okt/*` there). Codex has no per-agent tool allowlist.
export const MCP_SERVER = "okt";

// ---------------------------------------------------------------------------
// Tiny YAML frontmatter parser/serializer (good enough for our flat string map)
// ---------------------------------------------------------------------------

export const FRONTMATTER_RE = /^---\r?\n([\s\S]*?)\r?\n---\r?\n([\s\S]*)$/;

export function parseFrontmatter(text) {
  const m = text.match(FRONTMATTER_RE);
  if (!m) return { frontmatter: {}, body: text };
  const fmText = m[1];
  const body = m[2];
  const frontmatter = {};
  for (const rawLine of fmText.split(/\r?\n/)) {
    if (!rawLine.trim() || rawLine.trim().startsWith("#")) continue;
    const idx = rawLine.indexOf(":");
    if (idx === -1) continue;
    const key = rawLine.slice(0, idx).trim();
    let val = rawLine.slice(idx + 1).trim();
    if (
      (val.startsWith('"') && val.endsWith('"')) ||
      (val.startsWith("'") && val.endsWith("'"))
    ) {
      val = val.slice(1, -1);
    }
    frontmatter[key] = val;
  }
  return { frontmatter, body };
}

export function yamlEscape(s) {
  // Single-line YAML scalar; quote if it contains a colon, quote, or leading
  // special char. We always quote descriptions (they're long prose).
  if (/[:#"'`]/.test(s) || /^[&*!|>%@`-]/.test(s) || s.includes("\n")) {
    return `"${s.replace(/\\/g, "\\\\").replace(/"/g, '\\"')}"`;
  }
  return s;
}

export function serializeFrontmatter(map, order) {
  const lines = ["---"];
  for (const key of order) {
    if (map[key] === undefined || map[key] === null || map[key] === "") continue;
    lines.push(`${key}: ${yamlEscape(String(map[key]))}`);
  }
  // Any keys not in the prescribed order, appended alphabetically for stability.
  const extras = Object.keys(map)
    .filter((k) => !order.includes(k))
    .sort();
  for (const key of extras) {
    lines.push(`${key}: ${yamlEscape(String(map[key]))}`);
  }
  lines.push("---");
  return lines.join("\n");
}

// ---------------------------------------------------------------------------
// TOML helpers for the Codex agent files
// ---------------------------------------------------------------------------

export function tomlEscape(s) {
  // Multi-line basic string using triple-double-quotes; escape backslashes
  // and triple-quote sequences inside.
  const escaped = s
    .replace(/\\/g, "\\\\")
    .replace(/"""/g, '""\\"'); // break up any embedded triple-quotes
  return `"""${escaped}"""`;
}

// ---------------------------------------------------------------------------
// Per-format emitters
// ---------------------------------------------------------------------------

export function emitClaudeMd(name, src) {
  // Claude Code + VS Code/Copilot share the `.md` format (YAML frontmatter +
  // markdown body). We use the Claude `tools` syntax (comma-separated string
  // of tool names) which VS Code's agent loader also accepts per the
  // cross-tool compatibility notes.
  //
  // `okt` is the orchestrator (primary); the four subagents stay subagents.
  // Claude's frontmatter has no `mode` field — the orchestrator uses the
  // Task tool to delegate. We restrict `tools` to OKT MCP tools plus
  // read-only helpers so agents can't wander into editing the user's repo.
  const tools = `mcp__${MCP_SERVER}__*, Read, Grep, Glob, TodoWrite, Task, WebFetch, question`;
  const fm = {
    name,
    description: src.frontmatter.description,
    tools,
  };
  if (src.frontmatter.model) {
    // Claude Code doesn't understand `ollama/...`; `inherit` lets the user's
    // session model flow through. OKT agents are model-agnostic by design.
    fm.model = "inherit";
  }
  const order = ["name", "description", "tools", "model"];
  const fmText = serializeFrontmatter(fm, order);
  const header = [
    "<!--",
    "  Claude Code / VS Code Copilot agent. Generated from",
    "  .opencode/agent/" + name + ".md by ai-plugins/scripts/sync-agents.mjs.",
    "  Do not edit by hand — edit the source and re-run `just sync-agents`.",
    "-->",
  ].join("\n");
  return `${header}\n${fmText}\n\n${src.body.trimEnd()}\n`;
}

export function emitCodexToml(name, src) {
  // Codex custom-agent TOML. Required: name, description, developer_instructions.
  // No per-agent tool allowlist — sandbox_mode + mcp server scoping handle it.
  // The okt-setup skill (or the user's config.toml) registers the MCP server.
  const lines = [
    `name = ${JSON.stringify(name)}`,
    `description = ${JSON.stringify(src.frontmatter.description)}`,
    `model_reasoning_effort = "high"`,
    `sandbox_mode = "read-only"`,
    `developer_instructions = ${tomlEscape(src.body.trim())}`,
  ];
  const header = [
    "# Codex CLI agent. Generated from",
    "# .opencode/agent/" + name + ".md by ai-plugins/scripts/sync-agents.mjs.",
    "# Do not edit by hand — edit the source and re-run `just sync-agents`.",
  ].join("\n");
  return `${header}\n${lines.join("\n")}\n`;
}

export function emitOpencodeMd(name, src) {
  // opencode format: preserves `mode` and `model` from the source. The body
  // is copied verbatim. We drop `description` only if it's identical (it is).
  const fm = {
    description: src.frontmatter.description,
  };
  if (src.frontmatter.mode) fm.mode = src.frontmatter.mode;
  if (src.frontmatter.model) fm.model = src.frontmatter.model;
  const order = ["description", "mode", "model"];
  const fmText = serializeFrontmatter(fm, order);
  const header = [
    "<!--",
    "  opencode agent. Generated from .opencode/agent/" + name + ".md",
    "  by ai-plugins/scripts/sync-agents.mjs. Do not edit by hand — edit the",
    "  source and re-run `just sync-agents`.",
    "-->",
  ].join("\n");
  return `${header}\n${fmText}\n\n${src.body.trimEnd()}\n`;
}

// ---------------------------------------------------------------------------
// Driver
// ---------------------------------------------------------------------------

export async function readSources() {
  const files = (await readdir(SRC_DIR)).filter((f) => f.endsWith(".md"));
  const out = [];
  for (const f of files) {
    const name = f.replace(/\.md$/, "");
    const text = await readFile(join(SRC_DIR, f), "utf8");
    const parsed = parseFrontmatter(text);
    if (!parsed.frontmatter.description) {
      throw new Error(`${f}: missing required frontmatter field 'description'`);
    }
    out.push({ name, ...parsed });
  }
  // Stable order: `okt` first (the orchestrator), rest alphabetical.
  out.sort((a, b) => {
    if (a.name === "okt") return -1;
    if (b.name === "okt") return 1;
    return a.name.localeCompare(b.name);
  });
  return out;
}

export async function writeIfChanged(path, content) {
  let prev = "";
  try {
    prev = await readFile(path, "utf8");
  } catch {
    // File did not exist.
  }
  if (prev === content) return false;
  await mkdir(dirname(path), { recursive: true });
  await writeFile(path, content, "utf8");
  return true;
}

export async function sync() {
  const sources = await readSources();

  // The .md format emitted into ai-plugins/agents/ targets BOTH Claude Code
  // and VS Code/Copilot. Claude Code reads `tools:` as a comma-separated
  // string; VS Code reads it as a YAML array. They are not perfectly
  // compatible. To keep ONE .md file per agent that serves both, we use the
  // Claude (comma-separated string) form — VS Code's agent loader also accepts
  // a comma-separated string per the cross-tool compatibility notes. If a
  // future VS Code version requires the array form, split into two files.
  const written = [];
  for (const src of sources) {
    const claudePath = join(OUT_AGENTS, `${src.name}.md`);
    const codexPath = join(OUT_AGENTS, `${src.name}.toml`);
    const opencodePath = join(OUT_OPENCODE, `${src.name}.md`);

    if (await writeIfChanged(claudePath, emitClaudeMd(src.name, src))) {
      written.push(claudePath);
    }
    if (await writeIfChanged(codexPath, emitCodexToml(src.name, src))) {
      written.push(codexPath);
    }
    if (await writeIfChanged(opencodePath, emitOpencodeMd(src.name, src))) {
      written.push(opencodePath);
    }
  }

  // Prune any generated files in the output dirs that no longer have a source.
  const srcNames = new Set(sources.map((s) => s.name));
  for (const dir of [OUT_AGENTS, OUT_OPENCODE]) {
    if (!existsSync(dir)) continue;
    const entries = await readdir(dir);
    for (const entry of entries) {
      const base = entry.replace(/\.(md|toml)$/, "");
      if (!srcNames.has(base)) {
        await rm(join(dir, entry));
        written.push(`(removed) ${join(dir, entry)}`);
      }
    }
  }

  return { written, sources };
}

async function main() {
  const check = process.argv.includes("--check");
  const { written } = await sync();

  if (check && written.length > 0) {
    console.error(
      "sync-agents: drift detected. Re-run `just sync-agents` and commit.\n" +
        "Changed files:\n  " +
        written.map((p) => p.replace(ROOT + "/", "")).join("\n  ")
    );
    process.exit(1);
  }

  if (written.length === 0) {
    console.log("sync-agents: up to date.");
  } else {
    console.log(
      "sync-agents: wrote " +
        written.length +
        " file(s):\n  " +
        written.map((p) => p.replace(ROOT + "/", "")).join("\n  ")
    );
  }
}

// Only run main when invoked directly, not when imported by tests.
const isMain = import.meta.url === `file://${process.argv[1]}`;
if (isMain) {
  main().catch((err) => {
    console.error("sync-agents:", err);
    process.exit(1);
  });
}