---
id: ai-plugins
sidebar_position: 2
title: AI Client Plugins
---

# AI Client Plugins (agents)

OKT ships a single plugin that installs the six **Open Knowledge Tree** research
agents into AI coding clients that support plugin packages. The agents are
model-agnostic and talk to the OKT backend through the
[MCP server](/docs/mcp/getting-started) — no MCP URL is baked into the plugin.

| Agent | Role | Mode |
|---|---|---|
| `okt` | Orchestrator — entry point for research workflows | primary |
| `research` | Plans + gathers evidence (graph exploration, ingestion) | subagent |
| `investigation` | Creates investigations, ingests sources, tracks drain | subagent |
| `synthesizer` | Standalone research document on one scope | subagent |
| `super-synthesizer` | Meta-synthesis across multiple sub-syntheses | subagent |
| `reviewer` | Audits a synthesis for epistemic correctness + neutrality | subagent |

Clients with a first-class plugin package:

- **Claude Code** — via this repo as a Claude plugin marketplace
- **GitHub Copilot in VS Code** — via this repo as a Copilot plugin marketplace
- **OpenAI Codex CLI** — via this repo as a Codex plugin marketplace

Any other MCP-speaking client can be wired by hand — see
[Other clients](#other-clients-no-plugin-package) below.

After installing the plugin in your client, run the bundled `okt-setup` skill
to write the right MCP config block, then [authenticate with
OAuth 2.1](/docs/mcp/getting-started#how-an-mcp-client-authenticates) on first
use.

## Install

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

## Other clients (no plugin package)

If your AI client speaks MCP but has no OKT plugin package (e.g. Claude
Desktop, Cursor, Windsurf, Goose, Continue, a custom agent harness), you can
wire it up by hand:

1. **Point your client at the OKT MCP server.** Add an HTTP MCP server named
   `okt` pointing at `http://localhost:8080/api/v1/mcp` (or your remote OKT
   URL). See [Getting Started with MCP](/docs/mcp/getting-started#connecting-common-clients)
   for per-client snippets (Claude Desktop, Cursor, VS Code, MCP Inspector).
2. **Create the six agents manually** from the prompt files below. Each client
   has its own "agent"/"custom instruction"/"prompt" mechanism — copy the body
   of the matching prompt file into a new agent named `okt`, `research`,
   `investigation`, `synthesizer`, `super-synthesizer`, and `reviewer`
   respectively. Set `okt` as primary/standalone and the other five as
   subagents/delegates if your client distinguishes.
3. **Authenticate via OAuth 2.1** on first call — your client drives the flow
   automatically; see
   [Getting Started with MCP → How an MCP client authenticates](/docs/mcp/getting-started#how-an-mcp-client-authenticates).

## Agent prompts

The canonical agent prompt files live in the repo under
[`.opencode/agent/`](https://github.com/openktree/open-knowledge-tree/tree/main/.opencode/agent).
Each file is a self-contained system prompt (frontmatter + body); copy the
body into your client's agent/custom-instruction editor. The files are
identical across clients — only the frontmatter shape differs, which your
client ignores if it has its own agent config format.

| Agent | Prompt file | Description |
|---|---|---|
| `okt` | [`okt.md`](https://github.com/openktree/open-knowledge-tree/blob/main/.opencode/agent/okt.md) | Orchestrator — entry point for research workflows |
| `research` | [`research.md`](https://github.com/openktree/open-knowledge-tree/blob/main/.opencode/agent/research.md) | Plans + gathers evidence (graph exploration, ingestion) |
| `investigation` | [`investigation.md`](https://github.com/openktree/open-knowledge-tree/blob/main/.opencode/agent/investigation.md) | Creates investigations, ingests sources, tracks drain |
| `synthesizer` | [`synthesizer.md`](https://github.com/openktree/open-knowledge-tree/blob/main/.opencode/agent/synthesizer.md) | Standalone research document on one scope |
| `super-synthesizer` | [`super-synthesizer.md`](https://github.com/openktree/open-knowledge-tree/blob/main/.opencode/agent/super-synthesizer.md) | Meta-synthesis across multiple sub-syntheses |
| `reviewer` | [`reviewer.md`](https://github.com/openktree/open-knowledge-tree/blob/main/.opencode/agent/reviewer.md) | Audits a synthesis for epistemic correctness + neutrality |

The `okt` agent is the primary entry point — users invoke it directly and it
delegates to the five subagents as needed. If your client only supports a
single agent/prompt, start with `okt.md`; it can still call the MCP tools
directly for lightweight workflows, it just can't spawn the specialized
subagents.

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