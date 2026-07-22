// scripts/validate-manifests.test.mjs
//
// Schema validation for the plugin manifests, marketplace files, the
// okt-setup skill frontmatter, and the generated agents/*.md + agents/*.toml
// shapes. Run with:
//   node --test ai-plugins/scripts/validate-manifests.test.mjs
//
// Failures here mean either a manifest is malformed or a generated agent file
// is missing a field the target client requires.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFile, readdir } from "node:fs/promises";
import { existsSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { spawnSync } from "node:child_process";

const __dirname = dirname(fileURLToPath(import.meta.url));
const PLUGINS = join(__dirname, "..");
const ROOT = join(__dirname, "..", "..");

async function readJson(p) {
  return JSON.parse(await readFile(p, "utf8"));
}

// ---------------------------------------------------------------------------
// Shared plugin.json shape — all three manifests must agree on name/version.
// ---------------------------------------------------------------------------

const MANIFEST_PATHS = [
  join(PLUGINS, ".claude-plugin", "plugin.json"),
  join(PLUGINS, ".codex-plugin", "plugin.json"),
  join(PLUGINS, "plugin.json"), // VS Code / Copilot (root)
];

const EXPECTED_NAME = "okt-agents";
const EXPECTED_VERSION = "0.1.0";

test("all three plugin.json files exist", () => {
  for (const p of MANIFEST_PATHS) {
    assert.equal(existsSync(p), true, `missing manifest: ${p}`);
  }
});

test("all three plugin.json files share the same name and version", async () => {
  const manifests = await Promise.all(MANIFEST_PATHS.map((p) => readJson(p)));
  for (const m of manifests) {
    assert.equal(m.name, EXPECTED_NAME);
    assert.equal(m.version, EXPECTED_VERSION);
  }
});

test("each plugin.json points to existing agents/ and skills/ dirs", async () => {
  for (const p of MANIFEST_PATHS) {
    const m = await readJson(p);
    const agentsDir = join(PLUGINS, m.agents.replace(/^\.\//, ""));
    const skillsDir = join(PLUGINS, m.skills.replace(/^\.\//, ""));
    assert.equal(existsSync(agentsDir), true, `${p}: agents dir missing`);
    assert.equal(existsSync(skillsDir), true, `${p}: skills dir missing`);
  }
});

// ---------------------------------------------------------------------------
// Marketplace files
// ---------------------------------------------------------------------------

test("Claude marketplace.json references the okt-agents plugin", async () => {
  const m = await readJson(join(PLUGINS, ".claude-plugin", "marketplace.json"));
  assert.equal(m.name, "okt-agents-official");
  const plugin = m.plugins?.[0];
  assert.ok(plugin, "marketplace must list at least one plugin");
  assert.equal(plugin.name, EXPECTED_NAME);
});

test("Codex marketplace.json references the okt-agents plugin", async () => {
  const p = join(ROOT, ".agents", "plugins", "marketplace.json");
  assert.equal(existsSync(p), true, "repo-root .agents/plugins/marketplace.json missing");
  const m = await readJson(p);
  assert.equal(m.name, "okt-agents-official");
  const plugin = m.plugins?.[0];
  assert.ok(plugin, "Codex marketplace must list at least one plugin");
  assert.equal(plugin.name, EXPECTED_NAME);
  assert.ok(plugin.source, "Codex plugin entry must have a source");
});

// ---------------------------------------------------------------------------
// okt-setup skill frontmatter — opencode requires name == directory name.
// ---------------------------------------------------------------------------

test("okt-setup SKILL.md has name matching its directory", async () => {
  const skillPath = join(PLUGINS, "skills", "okt-setup", "SKILL.md");
  assert.equal(existsSync(skillPath), true, "okt-setup/SKILL.md missing");
  const text = await readFile(skillPath, "utf8");
  const m = text.match(/^---\n([\s\S]*?)\n---/m);
  assert.ok(m, "SKILL.md must have YAML frontmatter");
  const fm = m[1];
  assert.match(fm, /^name: okt-setup$/m, "skill name must be 'okt-setup' and match dir");
  assert.match(fm, /^description: .+$/m, "skill must have a description");
});

// ---------------------------------------------------------------------------
// Generated agents/*.md — Claude + VS Code Copilot format
// ---------------------------------------------------------------------------

test("agents/ has a .md file for each known OKT agent", async () => {
  const dir = join(PLUGINS, "agents");
  const names = ["okt", "research", "investigation", "synthesizer", "super-synthesizer"];
  for (const n of names) {
    const p = join(dir, `${n}.md`);
    assert.equal(existsSync(p), true, `missing ${p}`);
  }
});

test("each agents/*.md has name + description frontmatter", async () => {
  const dir = join(PLUGINS, "agents");
  const files = (await readdir(dir)).filter((f) => f.endsWith(".md"));
  assert.ok(files.length >= 5, `expected ≥5 agent .md files, got ${files.length}`);
  for (const f of files) {
    const text = await readFile(join(dir, f), "utf8");
    const m = text.match(/^---\n([\s\S]*?)\n---/m);
    assert.ok(m, `${f}: missing YAML frontmatter`);
    const fm = m[1];
    assert.match(fm, /^name: \S+$/m, `${f}: missing name`);
    assert.match(fm, /^description: .+$/m, `${f}: missing description`);
    // tools field is required for Claude agents (restricts to OKT MCP).
    assert.match(fm, /^tools: .+$/m, `${f}: missing tools`);
    assert.match(fm, /mcp__okt__\*/, `${f}: tools must include the mcp__okt__* glob`);
  }
});

// ---------------------------------------------------------------------------
// Generated agents/*.toml — Codex CLI format. Validated via Python tomllib.
// ---------------------------------------------------------------------------

function parseTomlFile(p) {
  // Use python tomllib to parse, then emit JSON for reliable cross-language handoff.
  const content = readFileSync(p, "utf8");
  const r = spawnSync(
    "python3",
    ["-c", "import sys,json,tomllib; print(json.dumps(tomllib.loads(sys.stdin.read())))"],
    { input: content, encoding: "utf8" }
  );
  if (r.error) return null; // python not installed
  if (r.status !== 0) return { __invalid: true, stderr: r.stderr };
  try {
    return JSON.parse(r.stdout);
  } catch {
    return null;
  }
}

test("python3 is available for TOML validation (or skip)", () => {
  const r = spawnSync("python3", ["--version"]);
  if (r.error || r.status !== 0) {
    // Not a failure — just a skip marker. The .toml tests below also tolerate
    // python absence by checking the textual shape.
  }
});

test("each agents/*.toml has name + description + developer_instructions", async () => {
  const dir = join(PLUGINS, "agents");
  const files = (await readdir(dir)).filter((f) => f.endsWith(".toml"));
  assert.ok(files.length >= 5, `expected ≥5 agent .toml files, got ${files.length}`);
  for (const f of files) {
    const p = join(dir, f);
    const text = await readFile(p, "utf8");
    assert.match(text, /^name = ".+"$/m, `${f}: missing name`);
    assert.match(text, /^description = ".+"$/m, `${f}: missing description`);
    assert.match(text, /^developer_instructions = """[\s\S]+"""/m, `${f}: missing developer_instructions`);
    assert.match(text, /^sandbox_mode = "read-only"$/m, `${f}: agents must be read-only by default`);

    // If python is available, assert the file parses as valid TOML.
    const parsed = parseTomlFile(p);
    if (parsed && parsed.__invalid) {
      assert.fail(`${f}: invalid TOML\n${parsed.stderr}`);
    }
    if (parsed && !parsed.__invalid) {
      assert.ok(parsed.name, `${f}: parsed TOML missing name`);
      assert.ok(parsed.developer_instructions, `${f}: parsed TOML missing developer_instructions`);
      assert.equal(parsed.sandbox_mode, "read-only");
    }
  }
});

// ---------------------------------------------------------------------------
// opencode/agents/*.md — preserves mode/model from source
// ---------------------------------------------------------------------------

test("opencode/agents/ has a copy of each agent", async () => {
  const dir = join(PLUGINS, "opencode", "agents");
  const names = ["okt", "research", "investigation", "synthesizer", "super-synthesizer"];
  for (const n of names) {
    const p = join(dir, `${n}.md`);
    assert.equal(existsSync(p), true, `missing ${p}`);
  }
});

test("opencode/agents/okt.md has mode: primary", async () => {
  const text = await readFile(join(PLUGINS, "opencode", "agents", "okt.md"), "utf8");
  assert.match(text, /^mode: primary$/m);
});

test("opencode/agents/research.md has mode: subagent", async () => {
  const text = await readFile(join(PLUGINS, "opencode", "agents", "research.md"), "utf8");
  assert.match(text, /^mode: subagent$/m);
});

// ---------------------------------------------------------------------------
// Cross-format consistency: every .md has a matching .toml sibling and vice
// versa, and the opencode/ copy exists for each.
// ---------------------------------------------------------------------------

test("agent names are consistent across md/toml/opencode", async () => {
  const agentsDir = join(PLUGINS, "agents");
  const opencodeDir = join(PLUGINS, "opencode", "agents");

  const md = (await readdir(agentsDir)).filter((f) => f.endsWith(".md")).map((f) => f.replace(/\.md$/, ""));
  const toml = (await readdir(agentsDir)).filter((f) => f.endsWith(".toml")).map((f) => f.replace(/\.toml$/, ""));
  const oc = (await readdir(opencodeDir)).filter((f) => f.endsWith(".md")).map((f) => f.replace(/\.md$/, ""));

  md.sort();
  toml.sort();
  oc.sort();
  assert.deepEqual(md, toml, "agents/ .md and .toml names must match");
  assert.deepEqual(md, oc, "agents/ and opencode/agents/ names must match");
});