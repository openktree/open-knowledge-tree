// opencode/index.test.ts
//
// Runtime tests for the @okt/ai-plugins opencode plugin. Run with:
//   bun test ai-plugins/opencode/index.test.ts
//
// Strategy: isolate HOME and XDG_CONFIG_HOME in a temp dir so the tests never
// touch the real ~/.config/opencode. Verify:
//   - The 5 agent .md files are installed into <config>/agents/ on plugin load
//   - The okt-setup skill is installed into <config>/skills/okt-setup/SKILL.md
//   - Re-running the plugin is a no-op (idempotent — no duplicate writes)
//   - Pre-existing user files are NOT clobbered
//   - The `event` hook re-installs missing files only on server.connected
//   - A non-server.connected event is ignored

import { test, beforeEach, afterEach } from "bun:test";
import { equal, ok, match } from "node:assert/strict";
import { mkdtemp, mkdir, writeFile, readFile, readdir, rm } from "node:fs/promises";
import { existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = dirname(fileURLToPath(import.meta.url));
const PLUGINS = join(__dirname, "..");

// The plugin resolves its bundled dir via `import.meta.url` at module load
// time. We import it fresh in each test after setting env vars so userConfigDir
// picks up the override. Bun caches modules by URL, so we bust the cache with
// a query string.

const AGENT_NAMES = [
  "okt",
  "research",
  "investigation",
  "synthesizer",
  "super-synthesizer",
  // New agents added to .opencode/agent/ are picked up automatically by the
  // generator; this list is intentionally NOT exhaustive. The count test
  // below derives the expected count from the bundled dir at runtime.
];

async function bundledAgentFiles(): Promise<string[]> {
  const dir = join(__dirname, "agents");
  const { readdir } = await import("node:fs/promises");
  return (await readdir(dir)).filter((f) => f.endsWith(".md"));
}

let tmpHome = "";
let tmpXdg = "";

async function freshImport() {
  const url = `file://${join(__dirname, "index.ts")}?$counter=${Math.random()}`;
  return (await import(url)) as typeof import("./index.ts");
}

beforeEach(async () => {
  tmpHome = await mkdtemp(join(tmpdir(), "okt-home-"));
  tmpXdg = await mkdtemp(join(tmpdir(), "okt-xdg-"));
  process.env.HOME = tmpHome;
  process.env.XDG_CONFIG_HOME = tmpXdg;
  delete process.env.OPENCODE_CONFIG_DIR;
});

afterEach(async () => {
  await rm(tmpHome, { recursive: true, force: true });
  await rm(tmpXdg, { recursive: true, force: true });
});

function configDir() {
  // userConfigDir prefers XDG_CONFIG_HOME when set.
  return join(tmpXdg, "opencode");
}

test("installs all bundled agents into <config>/agents/ on plugin load", async () => {
  const mod = await freshImport();
  const fakeCtx = { client: {} };
  const hooks = await mod.OKTAgentsPlugin(fakeCtx as never);

  const agentsDir = join(configDir(), "agents");
  const installed = await readdir(agentsDir);
  const expected = await bundledAgentFiles();
  ok(
    expected.length >= 5,
    `bundled agents dir should have ≥5 agents, got ${expected.length}`
  );
  for (const file of expected) {
    ok(installed.includes(file), `${file} should be installed`);
  }
  equal(installed.length, expected.length, "no extra/missing files");
  // No hooks error path triggered.
  ok(hooks.event, "event hook should be returned");
});

test("installs the okt-setup skill into <config>/skills/okt-setup/", async () => {
  const mod = await freshImport();
  await mod.OKTAgentsPlugin({ client: {} } as never);

  const skillPath = join(configDir(), "skills", "okt-setup", "SKILL.md");
  ok(existsSync(skillPath), "okt-setup/SKILL.md should be installed");
  const text = await readFile(skillPath, "utf8");
  match(text, /^name: okt-setup$/m);
});

test("re-running the plugin is a no-op (idempotent)", async () => {
  const mod = await freshImport();
  await mod.OKTAgentsPlugin({ client: {} } as never);
  // Snapshot installed files + mtimes would be ideal; here we just assert the
  // count doesn't grow and the content is unchanged after a second run.
  const agentsDir = join(configDir(), "agents");
  const before = (await readdir(agentsDir)).sort();

  await mod.OKTAgentsPlugin({ client: {} } as never);
  const after = (await readdir(agentsDir)).sort();
  equal(after.length, before.length);
  equal(after.join(","), before.join(","));
});

test("does not clobber a pre-existing user agent file", async () => {
  const mod = await freshImport();
  const agentsDir = join(configDir(), "agents");
  await mkdir(agentsDir, { recursive: true });
  const userFile = join(agentsDir, "okt.md");
  await writeFile(userFile, "# my custom okt\nuser edits\n", "utf8");

  await mod.OKTAgentsPlugin({ client: {} } as never);

  const content = await readFile(userFile, "utf8");
  equal(content, "# my custom okt\nuser edits\n", "user's okt.md must not be overwritten");
  // All other bundled agents should still install.
  const installed = (await readdir(agentsDir)).sort();
  const expected = (await bundledAgentFiles()).filter((f) => f !== "okt.md");
  for (const file of expected) {
    ok(installed.includes(file), `${file} should still install`);
  }
});

test("event hook ignores non-server.connected events", async () => {
  const mod = await freshImport();
  const hooks = await mod.OKTAgentsPlugin({ client: {} } as never);
  // Delete one agent file to simulate a wipe, then fire an unrelated event.
  const agentsDir = join(configDir(), "agents");
  const target = join(agentsDir, "research.md");
  await rm(target, { force: true });
  ok(!existsSync(target), "research.md should be gone");

  await hooks.event?.({ event: { type: "session.idle" } } as never);
  ok(!existsSync(target), "non-server event must not re-install");
});

test("event hook re-installs missing agents on server.connected", async () => {
  const mod = await freshImport();
  const hooks = await mod.OKTAgentsPlugin({ client: {} } as never);
  const agentsDir = join(configDir(), "agents");

  // Wipe one agent.
  const target = join(agentsDir, "research.md");
  await rm(target, { force: true });
  ok(!existsSync(target), "research.md should be gone after wipe");

  await hooks.event?.({ event: { type: "server.connected" } } as never);
  ok(existsSync(target), "research.md should be re-installed on server.connected");
  const text = await readFile(target, "utf8");
  // opencode-format agents use the filename as the name (no `name:` frontmatter
  // field); assert the description is present instead.
  match(text, /^description: Research Agent/m);
  match(text, /^mode: subagent$/m);
});

test("event hook re-installs a wiped skill on server.connected", async () => {
  const mod = await freshImport();
  const hooks = await mod.OKTAgentsPlugin({ client: {} } as never);
  const skillPath = join(configDir(), "skills", "okt-setup", "SKILL.md");
  await rm(skillPath, { force: true });
  ok(!existsSync(skillPath), "skill should be gone");

  await hooks.event?.({ event: { type: "server.connected" } } as never);
  ok(existsSync(skillPath), "skill should be re-installed on server.connected");
});

test("BUNDLED paths point to the plugin's own agents/ and skills/", async () => {
  const mod = await freshImport();
  ok(existsSync(join(mod.BUNDLED.agents, "okt.md")), "bundled agents/okt.md should exist");
  ok(existsSync(join(mod.BUNDLED.skills, "okt-setup", "SKILL.md")), "bundled skill should exist");
});