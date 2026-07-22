// @okt/ai-plugins — opencode plugin
//
// opencode has no native "plugin ships agents" mechanism, so this plugin
// installs the bundled OKT agent definitions into the user's global agents
// directory on first run. It is idempotent: existing files are never
// overwritten, so user edits are preserved. The bundled `okt-setup` skill is
// also installed so users can configure their OKT MCP server interactively.
//
// Add to your opencode.json:
//   { "plugin": ["@okt/ai-plugins"] }
//
// Then (or after a restart) ask any agent to run the `okt-setup` skill to
// connect to your OKT backend.

import { mkdir, copyFile, access, readdir } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { homedir } from "node:os";

import type { Plugin } from "@opencode-ai/plugin";

const __dirname = dirname(fileURLToPath(import.meta.url));
// index.ts lives at ai-plugins/opencode/index.ts. The opencode-format agent
// copies live alongside it at ai-plugins/opencode/agents/ (generated from the
// single source at .opencode/agent/*.md). The bundled skill lives one level
// up at ai-plugins/skills/okt-setup/SKILL.md (shared with the other clients'
// manifests). When published via npm, package.json's `files` array ships the
// whole opencode/ dir + skills/, so these relative paths still resolve.
const BUNDLED_AGENTS_DIR = join(__dirname, "agents");
const BUNDLED_SKILLS_DIR = join(__dirname, "..", "skills");

// Exported for tests so they can point at the bundled fixtures without
// re-deriving the path.
export const BUNDLED = {
  agents: BUNDLED_AGENTS_DIR,
  skills: BUNDLED_SKILLS_DIR,
};

export function userConfigDir(): string {
  const override =
    process.env.XDG_CONFIG_HOME || process.env.OPENCODE_CONFIG_DIR;
  if (override) return join(override, "opencode");
  return join(homedir(), ".config", "opencode");
}

export async function pathExists(p: string): Promise<boolean> {
  try {
    await access(p);
    return true;
  } catch {
    return false;
  }
}

interface DirentLike {
  name: string;
  isDirectory(): boolean;
}

// Copy every file from srcDir into destDir, skipping any file that already
// exists (never clobber user edits). Returns the list of newly installed
// relative paths. Handles one level of subdirectory nesting (for
// skills/<name>/SKILL.md).
export async function installTree(
  srcDir: string,
  destDir: string
): Promise<string[]> {
  const installed: string[] = [];
  await mkdir(destDir, { recursive: true });

  let entries: DirentLike[] = [];
  try {
    entries = (await readdir(srcDir, { withFileTypes: true })) as DirentLike[];
  } catch {
    return installed;
  }

  for (const entry of entries) {
    const srcPath = join(srcDir, entry.name);
    if (entry.isDirectory()) {
      let subEntries: DirentLike[] = [];
      try {
        subEntries = (await readdir(srcPath, {
          withFileTypes: true,
        })) as DirentLike[];
      } catch {
        continue;
      }
      for (const sub of subEntries) {
        if (sub.isDirectory()) {
          // Nested folder (e.g. skills/<name>/<sub>/...). Recurse into it.
          const subSrc = join(srcPath, sub.name);
          const subDest = join(destDir, entry.name, sub.name);
          await installTree(subSrc, subDest);
        } else {
          const destSubDir = join(destDir, entry.name);
          await mkdir(destSubDir, { recursive: true });
          const destFile = join(destSubDir, sub.name);
          if (!(await pathExists(destFile))) {
            await copyFile(join(srcPath, sub.name), destFile);
            installed.push(join(entry.name, sub.name));
          }
        }
      }
    } else {
      const destFile = join(destDir, entry.name);
      if (!(await pathExists(destFile))) {
        await copyFile(srcPath, destFile);
        installed.push(entry.name);
      }
    }
  }
  return installed;
}

export async function ensureSkillInstalled(
  skillName: string
): Promise<boolean> {
  // Install skills/<skillName>/SKILL.md into
  // ~/.config/opencode/skills/<skillName>/SKILL.md
  const srcSkill = join(BUNDLED_SKILLS_DIR, skillName, "SKILL.md");
  if (!(await pathExists(srcSkill))) return false;
  const destSkillDir = join(userConfigDir(), "skills", skillName);
  const destSkill = join(destSkillDir, "SKILL.md");
  if (await pathExists(destSkill)) return false;
  await mkdir(destSkillDir, { recursive: true });
  await copyFile(srcSkill, destSkill);
  return true;
}

type LogLevel = "info" | "warn" | "error";

// Best-effort structured logging via the opencode SDK client. Never throws —
// logging failures must not break plugin load.
async function log(
  client: unknown,
  level: LogLevel,
  message: string,
  extra?: Record<string, unknown>
): Promise<void> {
  const c = client as {
    app?: {
      log?: (args: {
        body: {
          service: string;
          level: LogLevel;
          message: string;
          extra?: Record<string, unknown>;
        };
      }) => Promise<unknown>;
    };
  } | undefined;
  if (!c?.app?.log) return;
  try {
    await c.app.log({
      body: { service: "@okt/ai-plugins", level, message, extra },
    });
  } catch {
    // Ignore — logging is best-effort.
  }
}

export const OKT_AGENTS_SKILL = "okt-setup";

export const OKTAgentsPlugin: Plugin = async (ctx) => {
  const { client } = ctx;
  const agentsDir = join(userConfigDir(), "agents");

  // Install once at plugin load. Best-effort: failures are logged but never
  // thrown, so a bad filesystem state can't break opencode startup.
  let installedAgents: string[] = [];
  let installedSkill = false;
  try {
    installedAgents = await installTree(BUNDLED_AGENTS_DIR, agentsDir);
    installedSkill = await ensureSkillInstalled(OKT_AGENTS_SKILL);
  } catch (err) {
    await log(client, "error", "Failed to install OKT agents/skill", {
      error: err instanceof Error ? err.message : String(err),
    });
  }

  if (installedAgents.length > 0 || installedSkill) {
    await log(client, "info", "Installed OKT agents and setup skill", {
      agents: installedAgents,
      skillInstalled: installedSkill,
      hint: "Run the okt-setup skill to configure the OKT MCP server, then restart opencode.",
    });
  }

  // Re-check on server connect — idempotent, so safe to re-run. This catches
  // the case where the user wiped their config dir between sessions.
  return {
    event: async ({ event }) => {
      if (event.type !== "server.connected") return;
      try {
        const newly = await installTree(BUNDLED_AGENTS_DIR, agentsDir);
        const newSkill = await ensureSkillInstalled(OKT_AGENTS_SKILL);
        if (newly.length > 0 || newSkill) {
          await log(client, "info", "Installed OKT agents on server connect", {
            agents: newly,
            skillInstalled: newSkill,
          });
        }
      } catch (err) {
        await log(client, "error", "Install check failed", {
          error: err instanceof Error ? err.message : String(err),
        });
      }
    },
  };
};

export default OKTAgentsPlugin;