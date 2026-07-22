# OKT Agents — AI client plugin

A single plugin that installs the six **Open Knowledge Tree** research agents
into four AI coding clients:

| Agent | Role | Mode |
|---|---|---|
| `okt` | Orchestrator — entry point for research workflows | primary |
| `research` | Plans + gathers evidence (graph exploration, ingestion) | subagent |
| `investigation` | Creates investigations, ingests sources, tracks drain | subagent |
| `synthesizer` | Standalone research document on one scope | subagent |
| `super-synthesizer` | Meta-synthesis across multiple sub-syntheses | subagent |
| `reviewer` | Audits a synthesis for epistemic correctness + neutrality | subagent |

Supported clients:

- **opencode** — via the `@okt/ai-plugins` npm package (JS plugin auto-installs the agents on first run)
- **Claude Code** — via this repo as a Claude plugin marketplace
- **GitHub Copilot in VS Code** — via this repo as a Copilot plugin marketplace
- **OpenAI Codex CLI** — via this repo as a Codex plugin marketplace

> The agents are model-agnostic and use the OKT MCP server. No MCP URL is baked
> into the plugin — run the bundled `okt-setup` skill to configure your client.

---

## Install

### opencode

```jsonc
// opencode.json
{
  "$schema": "https://opencode.ai/config.json",
  "plugin": ["@okt/ai-plugins"]
}
```

On first launch the plugin copies the six agent files into
`~/.config/opencode/agents/` (idempotent — never overwrites your edits) and
installs the `okt-setup` skill into `~/.config/opencode/skills/`. Then ask any
agent:

> Run the `okt-setup` skill to configure my OKT MCP server.

Restart opencode after the skill writes the config.

### Claude Code

Add this repo as a marketplace and install:

```
/plugin marketplace add anomalyco/open-knowledge-tree-go
/plugin install okt-agents@okt-agents-official
```

Or install directly from a local checkout:

```
/plugin install /path/to/open-knowledge-tree-go/ai-plugins
```

Then ask the `okt` agent:

> Run the `okt-setup` skill to configure my OKT MCP server.

Restart Claude Code after the skill writes the config.

### GitHub Copilot in VS Code

1. Open Settings and add the repo to `chat.plugins.marketplaces`:
   `"anomalyco/open-knowledge-tree-go"`
2. Command Palette → **Chat: Install Plugin** → pick `okt-agents`
3. Ask any agent to run the `okt-setup` skill (or run `Chat: Run Skill` →
   `okt-setup`).

Restart VS Code after the skill writes `.vscode/mcp.json`.

### OpenAI Codex CLI

```
codex plugin marketplace add anomalyco/open-knowledge-tree-go
codex plugin install okt-agents@okt-agents-official
```

Then run the `okt-setup` skill (ask any Codex agent, or edit
`~/.codex/config.toml` directly):

```toml
[mcp_servers.okt]
url = "http://localhost:8080/api/v1/mcp"
# bearer_token_env_var = "OKT_TOKEN"   # if your instance requires auth
```

Restart Codex after editing the config.

---

## How it works

```
.opencode/agent/*.md        ← single source of truth (5 agents, opencode format)
        │
        ▼  ai-plugins/scripts/sync-agents.mjs
        ├─► ai-plugins/agents/<name>.md      (Claude Code + VS Code Copilot)
        ├─► ai-plugins/agents/<name>.toml   (Codex CLI)
        └─► ai-plugins/opencode/agents/<name>.md  (opencode-format copy)
```

The three plugin manifests at the root of `ai-plugins/` let the same directory
be installed as a plugin by all four clients:

- `.claude-plugin/plugin.json` — Claude Code
- `.codex-plugin/plugin.json` — Codex CLI
- `plugin.json` (root) — VS Code / Copilot

The MCP server URL is **not** shipped in a `.mcp.json`. Each user's OKT backend
URL differs (localhost in dev, a remote host in production), so a baked-in URL
would be wrong for almost everyone. Instead the `okt-setup` skill walks the user
through writing the right config block into their client's config file.

### Adding or editing an agent

1. Edit (or add) a file under `.opencode/agent/` — this is the canonical source.
2. Run `just sync-agents` to regenerate `ai-plugins/agents/` and
   `ai-plugins/opencode/agents/`.
3. Commit both the source and the generated files together.
4. CI runs `just check-plugins` to fail on drift.

---

## Testing

```bash
just sync-agents      # regenerate all generated files
just check-plugins    # CI gate: drift + manifest validation + unit tests
just test-plugins     # run the full test suite (unit + runtime)
```

- `ai-plugins/scripts/sync-agents.test.mjs` — `node:test` unit tests for the
  generator (frontmatter round-trip, emitter output, TOML validity, drift
  detection, pruning).
- `ai-plugins/scripts/validate-manifests.test.mjs` — `node:test` schema checks
  for the three `plugin.json` files, the two `marketplace.json` files, and the
  generated `agents/*.md` + `agents/*.toml` shapes.
- `ai-plugins/opencode/index.test.ts` — `bun test` runtime tests with an
  isolated `HOME`, verifying the opencode plugin installs agents idempotently,
  preserves user edits, and re-installs missing files on `server.connected`.

### Per-client smoke test (manual)

After `just dev` (API on :8080, frontend on :3000) and installing the plugin:

- **opencode**: `opencode` in a throwaway dir with `"plugin": ["@okt/ai-plugins"]`
  → `@okt` appears in the agent list; `okt-setup` in the skills list.
- **Claude Code**: `claude --plugin-dir ai-plugins` → `/agents` lists 5 agents
  under `okt-agents`.
- **VS Code/Copilot**: `Chat: Install Plugin From Source` → `ai-plugins/` → 5
  agents in the `@`-mention list.
- **Codex**: `codex plugin marketplace add .` → `codex plugin install
  okt-agents@okt-agents-official` → 5 agents available.

### End-to-end

With the plugin installed and the OKT backend running:

1. Ask `@okt list my repositories` — should call `getRepositories` via MCP.
2. Ask `@okt` to run the `okt-setup` skill with
   `http://localhost:8080/api/v1/mcp` — should write the right config block.
3. Run a LOOKUP workflow ("search facts about X in repo Y") — confirms the MCP
   tools are reachable through the installed agents.

---

## Repository layout

```
ai-plugins/
├── .claude-plugin/plugin.json       # Claude Code manifest
├── .claude-plugin/marketplace.json  # Claude marketplace entry
├── .codex-plugin/plugin.json        # Codex CLI manifest
├── plugin.json                      # VS Code / Copilot manifest (root)
├── .agents/plugins/marketplace.json # Codex marketplace entry (repo-level)
├── agents/
│   ├── okt.md          okt.toml
│   ├── research.md     research.toml
│   ├── investigation.md investigation.toml
│   ├── synthesizer.md  synthesizer.toml
│   └── super-synthesizer.md super-synthesizer.toml
├── skills/okt-setup/SKILL.md        # cross-client MCP config skill
├── opencode/
│   ├── index.ts                    # @okt/ai-plugins npm plugin
│   ├── index.test.ts               # runtime tests (bun test)
│   └── agents/*.md                 # opencode-format copies
├── scripts/
│   ├── sync-agents.mjs             # generator (run via `just sync-agents`)
│   ├── sync-agents.test.mjs        # generator unit tests (node:test)
│   └── validate-manifests.test.mjs # manifest schema tests (node:test)
├── tsconfig.json
├── package.json                    # npm publish target (@okt/ai-plugins)
└── README.md
```

## License

MIT