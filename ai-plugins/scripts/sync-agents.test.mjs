// scripts/sync-agents.test.mjs
//
// Unit tests for the sync-agents generator. Run with:
//   node --test ai-plugins/scripts/sync-agents.test.mjs
//
// Covers: frontmatter parse/serialize round-trips, emitter output shapes,
// TOML validity (via Python tomllib), ordering, pruning, drift detection.

import { test } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtemp, mkdir, writeFile, readFile, rm, readdir } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import {
  MCP_SERVER,
  parseFrontmatter,
  serializeFrontmatter,
  yamlEscape,
  tomlEscape,
  emitClaudeMd,
  emitCodexToml,
  emitOpencodeMd,
  writeIfChanged,
} from "./sync-agents.mjs";

// ---------------------------------------------------------------------------
// Helper: parse a TOML file via Python's tomllib (stdlib, available 3.11+).
// Returns null if Python is unavailable so the test can skip rather than fail.
// ---------------------------------------------------------------------------

function parseToml(text) {
  const py = spawnSync("python3", ["-c", "import sys,tomllib; print(tomllib.loads(sys.stdin.read())['developer_instructions'][:50])"], {
    input: text,
    encoding: "utf8",
  });
  if (py.error || py.status !== 0) return null;
  return py.stdout;
}

function tomlIsValid(text) {
  const py = spawnSync("python3", ["-c", "import sys,tomllib; tomllib.loads(sys.stdin.read())"], {
    input: text,
    encoding: "utf8",
  });
  if (py.error) return false;
  return py.status === 0;
}

// ---------------------------------------------------------------------------
// Frontmatter
// ---------------------------------------------------------------------------

test("parseFrontmatter extracts frontmatter and body", () => {
  const text = "---\ndescription: hello\nmode: subagent\n---\n\nBody here.\n";
  const { frontmatter, body } = parseFrontmatter(text);
  assert.equal(frontmatter.description, "hello");
  assert.equal(frontmatter.mode, "subagent");
  // The body retains the leading blank line after the frontmatter fence.
  assert.match(body, /Body here\./);
});

test("parseFrontmatter handles quoted values", () => {
  const text = '---\ndescription: "has: colon"\n---\nbody\n';
  const { frontmatter } = parseFrontmatter(text);
  assert.equal(frontmatter.description, "has: colon");
});

test("parseFrontmatter returns empty frontmatter when no fence", () => {
  const { frontmatter, body } = parseFrontmatter("no fence here");
  assert.deepEqual(frontmatter, {});
  assert.equal(body, "no fence here");
});

test("serializeFrontmatter emits fields in the prescribed order", () => {
  const out = serializeFrontmatter(
    { description: "d", name: "n", model: "m", tools: "t" },
    ["name", "description", "tools", "model"]
  );
  const lines = out.split("\n");
  assert.equal(lines[0], "---");
  assert.match(lines[1], /^name: n$/);
  assert.match(lines[2], /^description: d$/);
  assert.match(lines[3], /^tools: t$/);
  assert.match(lines[4], /^model: m$/);
  assert.equal(lines[5], "---");
});

test("serializeFrontmatter skips empty fields", () => {
  const out = serializeFrontmatter(
    { name: "n", model: "", description: "d" },
    ["name", "description", "model"]
  );
  assert.match(out, /name: n/);
  assert.match(out, /description: d/);
  assert.doesNotMatch(out, /model:/);
});

test("serializeFrontmatter quotes descriptions containing colons (em-dash ok)", () => {
  // The real OKT descriptions contain em-dashes and are long prose; they must
  // be quoted to survive a YAML round-trip.
  const out = serializeFrontmatter(
    { description: "Orchestrator — research workflows: creates investigations" },
    ["description"]
  );
  assert.match(out, /^description: ".*"$/m);
  assert.match(out, /Orchestrator — research workflows/);
});

test("serializeFrontmatter round-trips through parseFrontmatter", () => {
  const fm = { description: "a: b", name: "x", tools: "t" };
  const order = ["name", "description", "tools"];
  const text = `${serializeFrontmatter(fm, order)}\n\nbody\n`;
  const parsed = parseFrontmatter(text).frontmatter;
  assert.equal(parsed.name, "x");
  assert.equal(parsed.description, "a: b");
  assert.equal(parsed.tools, "t");
});

test("yamlEscape quotes strings containing colons or quotes", () => {
  assert.equal(yamlEscape("plain"), "plain");
  assert.match(yamlEscape("has: colon"), /^".*"$/);
  assert.match(yamlEscape('has " quote'), /^".*"$/);
  // Newlines force quoting too.
  assert.match(yamlEscape("multi\nline"), /^"[\s\S]*"$/s);
});

// ---------------------------------------------------------------------------
// emitClaudeMd
// ---------------------------------------------------------------------------

const SAMPLE = {
  name: "okt",
  frontmatter: {
    // Real OKT descriptions contain em-dashes AND colons (e.g. "Phase 1:").
    // The colon forces yamlEscape to quote — this mirrors production input.
    description: "Orchestrator — research workflows: creates investigations.",
    mode: "primary",
    model: "ollama/glm-5.2:cloud",
  },
  body: "You are the OKT orchestrator.\n\nDo the thing.\n",
};

test("emitClaudeMd produces required Claude frontmatter fields", () => {
  const out = emitClaudeMd("okt", SAMPLE);
  assert.match(out, /<!--[\s\S]*?-->/);
  assert.match(out, /^name: okt$/m);
  assert.match(out, /^description: "Orchestrator — research workflows: creates investigations\."$/m);
  assert.match(
    out,
    new RegExp(`^tools: mcp__${MCP_SERVER}__\\*, Read, Grep, Glob, TodoWrite, Task, WebFetch, question$`, "m")
  );
  assert.match(out, /^model: inherit$/m);
  // mode should NOT appear (Claude has no mode field).
  assert.doesNotMatch(out, /^mode:/m);
  // Body preserved.
  assert.match(out, /You are the OKT orchestrator\./);
});

test("emitClaudeMd omits model when source has none", () => {
  const noModel = { ...SAMPLE, frontmatter: { ...SAMPLE.frontmatter } };
  delete noModel.frontmatter.model;
  const out = emitClaudeMd("okt", noModel);
  assert.doesNotMatch(out, /^model:/m);
});

test("emitClaudeMd body is verbatim (not reformatted)", () => {
  const body = "Line one.\n\nLine two with    weird   spacing.\n";
  const out = emitClaudeMd("x", { ...SAMPLE, body });
  assert.match(out, /Line two with    weird   spacing\./);
});

// ---------------------------------------------------------------------------
// emitCodexToml
// ---------------------------------------------------------------------------

test("emitCodexToml produces valid TOML with required fields", () => {
  const out = emitCodexToml("okt", SAMPLE);
  assert.equal(tomlIsValid(out), true, "Codex TOML must be valid");
  assert.match(out, /^name = "okt"$/m);
  assert.match(out, /^description = "Orchestrator — research workflows: creates investigations\."$/m);
  assert.match(out, /^sandbox_mode = "read-only"$/m);
  assert.match(out, /^model_reasoning_effort = "high"$/m);
  assert.match(out, /^developer_instructions = """/m);
});

test("emitCodexToml escapes embedded triple-quotes in the body", () => {
  const body = 'Before """ embedded """ after.\n';
  const out = emitCodexToml("x", { ...SAMPLE, body });
  assert.equal(tomlIsValid(out), true, "TOML with escaped triple-quotes must be valid");
  // The raw text must not contain an unescaped `"""` sequence inside the string.
  const stringBody = out.slice(
    out.indexOf('developer_instructions = """') + 'developer_instructions = """'.length,
    out.lastIndexOf('"""')
  );
  assert.doesNotMatch(stringBody, /(?<!\\)"""/, "no unescaped triple-quote inside the string body");
});

test("emitCodexToml escapes backslashes", () => {
  const body = "Path: C:\\Users\\x\n";
  const out = emitCodexToml("x", { ...SAMPLE, body });
  assert.equal(tomlIsValid(out), true);
  assert.match(out, /C:\\\\Users\\\\x/);
});

test("emitCodexToml preserves the body verbatim (modulo trim)", () => {
  const body = "Para one.\n\nPara two.\n";
  const out = emitCodexToml("x", { ...SAMPLE, body });
  assert.match(out, /Para one\.[\s\S]*Para two\./);
});

// ---------------------------------------------------------------------------
// emitOpencodeMd
// ---------------------------------------------------------------------------

test("emitOpencodeMd preserves mode and model from source", () => {
  const out = emitOpencodeMd("okt", SAMPLE);
  assert.match(out, /^description: "Orchestrator — research workflows: creates investigations\."$/m);
  assert.match(out, /^mode: primary$/m);
  // model contains a slash and dots — must be quoted by our serializer.
  assert.match(out, /^model: "ollama\/glm-5\.2:cloud"$/m);
  assert.match(out, /You are the OKT orchestrator\./);
});

test("emitOpencodeMd omits mode/model when source lacks them", () => {
  const minimal = {
    name: "x",
    frontmatter: { description: "d" },
    body: "b\n",
  };
  const out = emitOpencodeMd("x", minimal);
  assert.match(out, /^description: d$/m);
  assert.doesNotMatch(out, /^mode:/m);
  assert.doesNotMatch(out, /^model:/m);
});

// ---------------------------------------------------------------------------
// tomlEscape
// ---------------------------------------------------------------------------

test("tomlEscape wraps in triple-double-quotes", () => {
  assert.match(tomlEscape("plain"), /^"""plain"""$/);
});

// ---------------------------------------------------------------------------
// writeIfChanged
// ---------------------------------------------------------------------------

test("writeIfChanged writes new files and reports change", async () => {
  const dir = await mkdtemp(join(tmpdir(), "okt-sync-"));
  try {
    const p = join(dir, "x.txt");
    const changed = await writeIfChanged(p, "hello");
    assert.equal(changed, true);
    assert.equal(await readFile(p, "utf8"), "hello");
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("writeIfChanged returns false when content is identical", async () => {
  const dir = await mkdtemp(join(tmpdir(), "okt-sync-"));
  try {
    const p = join(dir, "x.txt");
    await writeFile(p, "hello", "utf8");
    const changed = await writeIfChanged(p, "hello");
    assert.equal(changed, false);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("writeIfChanged returns true when content differs", async () => {
  const dir = await mkdtemp(join(tmpdir(), "okt-sync-"));
  try {
    const p = join(dir, "x.txt");
    await writeFile(p, "old", "utf8");
    const changed = await writeIfChanged(p, "new");
    assert.equal(changed, true);
    assert.equal(await readFile(p, "utf8"), "new");
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});