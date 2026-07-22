---
name: okt-setup
description: Configure the Open Knowledge Tree MCP server in the user's current AI coding client (opencode, Claude Code, GitHub Copilot in VS Code, or OpenAI Codex CLI). Use when OKT agents are installed but cannot reach the OKT backend, or when the user asks to "set up OKT", "configure the OKT MCP server", or "connect OKT".
license: MIT
compatibility: opencode
metadata:
  audience: okt-users
  component: mcp-configuration
---

## Goal

Add an HTTP MCP server named `okt` pointing at an Open Knowledge Tree backend to the
user's current client, then tell them to restart. Never ship a static URL — ask the
user for their OKT base URL first.

## Steps

1. **Ask for the OKT MCP URL.** Use the `question` tool (or ask in chat if no
   `question` tool exists). Default: `http://localhost:8080/api/v1/mcp`. Accept
   either the bare MCP endpoint or the OKT base URL (in which case append
   `/api/v1/mcp`). Do not assume a remote or hosted URL.

2. **Detect the active client.** Use these signals, in order:
   - The agent that invoked this skill is named `okt` and the running client is
     opencode → **opencode**.
   - The system prompt or environment mentions "Claude Code" → **claude-code**.
   - The system prompt or environment mentions "VS Code" / "Copilot" → **vscode**.
   - The system prompt or environment mentions "Codex" → **codex**.
   If you cannot detect, ask the user which client they are running.

3. **Write the configuration for the detected client.** Use the `write` tool (or
   the appropriate file-edit tool). The exact file and shape per client:

   ### opencode
   - User-global config: `~/.config/opencode/opencode.json`
   - Add (or merge into) the top-level `mcp` object:
     ```json
     {
       "mcp": {
         "okt": {
           "type": "remote",
           "url": "<OKT_MCP_URL>",
           "enabled": true,
           "timeout": 15000
         }
       }
     }
     ```
   - If a project-local `opencode.jsonc` exists and the user wants project-only
     scope, write there instead. Preserve existing JSON keys; only add/replace
     the `mcp.okt` entry.

   ### claude-code
   - Run `claude mcp add okt --transport http "<OKT_MCP_URL>"` via the `bash`
     tool (writes to the user config, no manual file path needed).
   - If the `claude` CLI is unavailable, fall back to writing the user config
     file at `~/.claude.json` (Claude Code stores MCP servers under the top-level
     `mcpServers` key):
     ```json
     { "mcpServers": { "okt": { "type": "http", "url": "<OKT_MCP_URL>" } } }
     ```

   ### vscode (GitHub Copilot)
   - Workspace (project): `.vscode/mcp.json` with the `servers` key:
     ```json
     { "servers": { "okt": { "type": "http", "url": "<OKT_MCP_URL>" } } }
     ```
   - User-global: Command Palette → **MCP: Open User Configuration**, same
     `servers` shape. Prefer workspace scope unless the user asks for global.

   ### codex (OpenAI Codex CLI)
   - Append a `[mcp_servers.okt]` table to `~/.codex/config.toml` (or
     `.codex/config.toml` for project scope). If the file already contains a
     `[mcp_servers.okt]` table, replace it. Example:
     ```toml
     [mcp_servers.okt]
     url = "<OKT_MCP_URL>"
     ```
   - If the OKT instance requires a bearer token, also add:
     ```toml
     bearer_token_env_var = "OKT_TOKEN"
     ```
     and tell the user to export `OKT_TOKEN` in their shell.

4. **Confirm and instruct a restart.** After writing, tell the user:
   - Which file was written (or which CLI command ran).
   - To **restart their client** so the MCP server is loaded.
   - That after restart they can verify by asking any OKT agent (e.g. `@okt
     list my repositories`) — the agent will use the `okt` MCP server tools
     automatically.

5. **Do not modify the agent files.** This skill only configures the MCP
   transport. The agents themselves are installed by the plugin; do not touch
   `agents/*.md` or `agents/*.toml`.

## What NOT to do

- Do not invent a remote URL. Always ask.
- Do not write `.mcp.json` into the OKT plugin directory itself — the plugin is
  distributed without a baked-in URL by design. Configuration lives in the
  user's client config, not in the plugin.
- Do not skip the restart reminder. MCP servers are loaded at client startup.