---
id: ai-plugins
sidebar_position: 2
title: AI Client Plugins
---

# AI Client Plugins (agents)

OKT ships a single plugin that installs the six **Open Knowledge Tree** research
agents into four AI coding clients. The agents are model-agnostic and talk to
the OKT backend through the [MCP server](/docs/mcp/getting-started) — no MCP
URL is baked into the plugin.

| Agent | Role | Mode |
|---|---|---|
| `okt` | Orchestrator — entry point for research workflows | primary |
| `research` | Plans + gathers evidence (graph exploration, ingestion) | subagent |
| `investigation` | Creates investigations, ingests sources, tracks drain | subagent |
| `synthesizer` | Standalone research document on one scope | subagent |
| `super-synthesizer` | Meta-synthesis across multiple sub-syntheses | subagent |
| `reviewer` | Audits a synthesis for epistemic correctness + neutrality | subagent |

Supported clients:

- **opencode** — via the `@okt/ai-plugins` npm package
- **Claude Code** — via this repo as a Claude plugin marketplace
- **GitHub Copilot in VS Code** — via this repo as a Copilot plugin marketplace
- **OpenAI Codex CLI** — via this repo as a Codex plugin marketplace

After installing the plugin in your client, run the bundled `okt-setup` skill
to write the right MCP config block, then [authenticate with
OAuth 2.1](/docs/mcp/getting-started#how-an-mcp-client-authenticates) on first
use.

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
/plugin marketplace add openktree/open-knowledge-tree
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

Install directly from the remote repo (no local checkout or marketplace config
needed):

1. Command Palette → **Chat: Install Plugin From Source** → enter:
   `https://github.com/openktree/open-knowledge-tree`
2. Ask any agent to run the `okt-setup` skill (or run `Chat: Run Skill` →
   `okt-setup`).

Restart VS Code after the skill writes `.vscode/mcp.json`.

<details>
<summary>Prefer a marketplace?</summary>

Add the repo as a marketplace and install from it:

1. Open Settings and add the repo to `chat.plugins.marketplaces`:
   `"openktree/open-knowledge-tree"`
2. Command Palette → **Chat: Install Plugin** → pick `okt-agents`
3. Ask any agent to run the `okt-setup` skill (or run `Chat: Run Skill` →
   `okt-setup`).

</details>

### OpenAI Codex CLI

```
codex plugin marketplace add openktree/open-knowledge-tree
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

## Next steps

Once the plugin is installed and the `okt-setup` skill has written your MCP
config:

1. Restart your AI client so it picks up the `okt` MCP server.
2. Invoke any OKT agent (e.g. `@okt list my repositories`). The first call
   triggers the [OAuth 2.1 authorize flow](/docs/mcp/getting-started#how-an-mcp-client-authenticates) —
   your client opens a browser to the OKT login + consent page, then caches the
   access token for subsequent calls.
3. Follow the typical agent workflow described in
   [MCP Overview](/docs/mcp/overview#the-typical-agent-workflow), or browse the
   full [MCP Tools Reference](/docs/mcp/tools).

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

The source of truth for the plugin lives in
[`ai-plugins/README.md`](https://github.com/openktree/open-knowledge-tree/blob/main/ai-plugins/README.md)
in the repo.