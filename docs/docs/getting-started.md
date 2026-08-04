---
id: getting-started
sidebar_position: 0
title: Getting Started
---

# Getting Started

Run the full OKT stack, register your first user, wire the OKT agents into your
AI coding client, and authenticate via OAuth 2.1. No git clone, no Go, no Node
— just Docker.

The end-to-end flow is:

1. **[Boot the stack](#1-configure-your-environment)** with `docker compose up`.
2. **[Register your first user](#3-register-your-first-user)** — the first
   account becomes the system admin.
3. **[Configure the OKT agents](#4-configure-the-okt-agents-in-your-ai-client)**
   in your AI coding client (Claude Code, VS Code Copilot, Codex, or any other
   agentic tool that speaks MCP).
4. **[Authenticate via OAuth 2.1](#5-authenticate-via-oauth-21)** on first use
   of an OKT agent.

## 1. Configure your environment

Create a folder for OKT, then create an `.env` file inside it with your API keys.

**Linux / macOS:**

```bash
mkdir okt && cd okt
```
**Windows:**
```powershell
mkdir okt; cd okt
```

Create a .env file and add the variables for openrouter and serper. This is required for LLM access, beware that running a research process
will result in token usage and cost, you can limit the token maximun consumption in openrouter api keys page. Example Variables file:

```
SERPER_API_KEY=<your-serper-key>
OPENROUTER_API_KEY=<your-openrouter-key>
OPENALEX_EMAIL=<your-email>
UNPAYWALL_EMAIL=<your-email>

# Optional: first-boot admin (see step 3 below).
# By default the FIRST user to register is auto-promoted to sysadmin
# — safe for localhost. For a public deployment, uncomment and set
OKT_BOOTSTRAP_AUTO_PROMOTE=true
OKT_BOOTSTRAP_DEFAULT_ADMIN_EMAIL=<your-email>
# OKT_BOOTSTRAP_DEFAULT_ADMIN_PASSWORD=<generate-a-strong-one>
# OKT_BOOTSTRAP_DEFAULT_ADMIN_DISPLAY_NAME=Default Admin
```

Replace the `<your-...>` values with real ones. You need **at minimum**:
- `SERPER_API_KEY` — web search to find sources.
- `OPENROUTER_API_KEY` — LLM for fact extraction, concept extraction, and synthesis

Lines starting with `#` are comments — you can delete any line you don't need.

See [Configuration Reference](/docs/reference/config) for all valid values and environment variable overrides.


## 2. Boot the stack

From inside the `okt` folder (where your `.env` lives):

```bash

curl -sSL https://raw.githubusercontent.com/openktree/open-knowledge-tree/main/docker-compose.yml > docker-compose.yml
docker compose up -d
```

This pulls pre-built images from GitHub Container Registry and starts everything:

| Service | Port | What it does |
|---------|------|-------------|
| **Frontend** | [localhost:3000](http://localhost:3000) | Browse facts, concepts, reports |
| **API** | localhost:8080 | REST API + MCP server |
| **Postgres** | 5432 | Application database |
| **Postgres (tasks)** | 5434 | Background job queue |
| **Qdrant** | 6333/6334 | Vector search |
| **FlareSolverr** ×3 | 8191–8193 | JS-challenge bypass |

## 3. Register your first user

Go to **[http://localhost:3000](http://localhost:3000)** and register. The
**first** account you create is automatically promoted to system admin
(sysadmin). A starter repository is created for you on first boot.

> For a **public** deployment, set `OKT_BOOTSTRAP_AUTO_PROMOTE=false` in
> `.env` and use the `OKT_BOOTSTRAP_DEFAULT_ADMIN_*` env vars to seed an
> explicit admin instead. See [Configuration Reference](/docs/reference/config).

## 4. Configure the OKT agents in your AI client

OKT ships a single plugin that installs the six research agents (`okt`,
`research`, `investigation`, `synthesizer`, `super-synthesizer`, `reviewer`)
into your AI coding client. The agents talk to the OKT backend through the
[MCP server](/docs/mcp/getting-started) at
`http://localhost:8080/api/v1/mcp`.

Pick your client below — full per-client instructions are in
[AI Client Plugins](/docs/mcp/ai-plugins):

- **Claude Code** — `/plugin marketplace add openktree/open-knowledge-tree`
  then `/plugin install okt-agents@okt-agents-official`.
- **GitHub Copilot in VS Code** — Command Palette →
  **Chat: Install Plugin From Source** → enter
  `https://github.com/openktree/open-knowledge-tree`.
- **OpenAI Codex CLI** — `codex plugin marketplace add openktree/open-knowledge-tree`
  then `codex plugin install okt-agents@okt-agents-official`.
- **Other agentic tools** — if your client speaks MCP but has no OKT plugin
  package, you can wire the MCP server by hand and create the agents manually
  from the [agent prompts](/docs/mcp/ai-plugins#agent-prompts). See
  [AI Client Plugins → Other clients](/docs/mcp/ai-plugins#other-clients-no-plugin-package).

After installing the plugin, ask any OKT agent to run the `okt-setup` skill so
it writes the right MCP config block into your client:

> Run the `okt-setup` skill to configure my OKT MCP server.

The skill asks for your OKT MCP URL (default
`http://localhost:8080/api/v1/mcp`) and writes the config to the right place
for your client. **Restart your AI client** afterwards so it picks up the `okt`
MCP server.

## 5. Authenticate via OAuth 2.1

The OKT MCP server is protected by OAuth 2.1 with PKCE. A compliant client
self-registers and runs the authorize/token flow on first connect — you
normally do nothing by hand.

1. In your AI client, invoke any OKT agent (e.g. `@okt list my repositories`).
2. The first call returns `401` and the client opens a browser to the OKT
   authorize endpoint.
3. Log in with the account you registered in step 3 and approve consent.
4. The client caches the access token and retries the call. Subsequent calls
   reuse the cached token; the client refreshes it automatically when it
   expires.

If your client doesn't drive the flow automatically, see
[Getting Started with MCP](/docs/mcp/getting-started#manual-flow-for-debugging-or-scripts)
for the manual `register → authorize → token` curl flow.

## Provider setup

### Serper (required)

[serper.dev](https://serper.dev) — Google web search API that finds candidate sources. The free tier is enough for getting started. Sign up, grab a key, paste it into `SERPER_API_KEY`.

### OpenRouter (required — or Ollama)

[openrouter.ai](https://openrouter.ai) — gives you a single API key for GPT, Claude, Gemini, Llama, and dozens of other models. OKT calls a chat model for fact decomposition, concept extraction, and synthesis. Sign up, add credits (a few dollars goes a long way), and paste the key into `OPENROUTER_API_KEY`.

Alternatively, if you run a local LLM, set `OLLAMA_API_KEY` instead.

### OpenAlex (optional, recommended)

[openalex.org](https://openalex.org) — free academic-works search provider. **Providing an email address** gets you into the "polite pool" with higher rate limits and better response times. Without an email, it works but may be throttled under load. Set `OPENALEX_EMAIL` to any real address — it's just a courtesy header, not an API key.

### Unpaywall (optional)

[unpaywall.org](https://unpaywall.org) — resolves DOI-tagged sources to open-access PDFs. Your email acts as the API key. Set `UNPAYWALL_EMAIL` to enable the open-access resolution tier; without it, DOIs are resolved via plain HTTP fetch.

### FlareSolverr (bundled, no config needed)

The compose file boots three [Byparr](https://github.com/ThePhaseless/Byparr) instances (FlareSolverr-compatible) that solve JavaScript challenges from Cloudflare, Datadome, and PerimeterX. No configuration required — they're wired up automatically.

## Pin a release

By default the compose file pulls `latest`. Pin a specific version for reproducibility — add this line to your `.env`:

```
OKT_TAG=v0.1.0
```

## Stop the stack

Press **Ctrl+C** in the terminal where it's running, or from another terminal:

```bash
docker compose -f https://raw.githubusercontent.com/openktree/open-knowledge-tree/main/docker-compose.yml down
```

Data persists in Docker volumes between restarts. Add `-v` to wipe everything:

```bash
docker compose -f https://raw.githubusercontent.com/openktree/open-knowledge-tree/main/docker-compose.yml down -v
```

## Developing from source

If you want hot-reload, source mounts, and the full dev toolchain (`just dev`), you'll need to clone the repo. See [Local Dev](/docs/local-dev/overview) for the developer-focused setup with Go, Node, and `just`.