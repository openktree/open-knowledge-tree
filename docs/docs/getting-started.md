---
id: getting-started
sidebar_position: 0
title: Getting Started
---

# Getting Started

Run the full OKT stack with two commands. No git clone, no Go, no Node — just Docker.

## 1. Create a config file

Create a folder for OKT, then create an `.env` file inside it with your API keys.

**Linux / macOS:**

```bash
mkdir okt && cd okt
```

Then copy this into a file called `.env` (use your editor, or paste from the terminal):

```
SERPER_API_KEY=<your-serper-key>
OPENROUTER_API_KEY=<your-openrouter-key>
OPENALEX_EMAIL=<your-email>
UNPAYWALL_EMAIL=<your-email>

# Optional: first-boot admin (see step 3 below).
# By default the FIRST user to register is auto-promoted to sysadmin
# — safe for localhost. For a public deployment, uncomment and set
# OKT_BOOTSTRAP_AUTO_PROMOTE=false plus the explicit admin below.
# OKT_BOOTSTRAP_AUTO_PROMOTE=true
# OKT_BOOTSTRAP_DEFAULT_ADMIN_EMAIL=admin@example.com
# OKT_BOOTSTRAP_DEFAULT_ADMIN_PASSWORD=<generate-a-strong-one>
# OKT_BOOTSTRAP_DEFAULT_ADMIN_DISPLAY_NAME=Default Admin
```

Replace the `<your-...>` values with real ones. You need **at minimum**:
- `SERPER_API_KEY` — web search to find sources
- `OPENROUTER_API_KEY` — LLM for fact extraction, concept extraction, and synthesis

Lines starting with `#` are comments — you can delete any line you don't need.

See [Configuration Reference](/docs/reference/config) for all valid values and environment variable overrides.

**Windows (PowerShell):**

```powershell
mkdir okt; cd okt
@"
SERPER_API_KEY=<your-serper-key>
OPENROUTER_API_KEY=<your-openrouter-key>
OPENALEX_EMAIL=<your-email>
UNPAYWALL_EMAIL=<your-email>
"@ | Out-File -Encoding utf8 .env
```

Or just open a text editor, paste the content above, and save as `.env` in the `okt` folder. Make sure the file is named `.env` and not `.env.txt`. The `#`-prefixed bootstrap lines are optional — see step 3 below.

## 2. Boot the stack

From inside the `okt` folder (where your `.env` lives):

```bash
docker compose -f https://raw.githubusercontent.com/openktree/open-knowledge-tree-go/main/docker-compose.yml up
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

## 3. Open the frontend

Go to **[http://localhost:3000](http://localhost:3000)** and register. The
**first** account you create is automatically promoted to system admin
(sysadmin) — this is safe on a localhost dev stack and is the smooth
out-of-the-box path: no env vars, no `psql` surgery, no scripts. A starter
repository is also created for you on first boot.

> For a **public** deployment, set `OKT_BOOTSTRAP_AUTO_PROMOTE=false` in
> `.env` and use the `OKT_BOOTSTRAP_DEFAULT_ADMIN_*` env vars to seed an
> explicit admin instead, so an attacker cannot become sysadmin by
> registering first. See [Configuration Reference](/docs/reference/config).

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
OKT_TAG=v1.0.0
```

## Stop the stack

Press **Ctrl+C** in the terminal where it's running, or from another terminal:

```bash
docker compose -f https://raw.githubusercontent.com/openktree/open-knowledge-tree-go/main/docker-compose.yml down
```

Data persists in Docker volumes between restarts. Add `-v` to wipe everything:

```bash
docker compose -f https://raw.githubusercontent.com/openktree/open-knowledge-tree-go/main/docker-compose.yml down -v
```

## Developing from source

If you want hot-reload, source mounts, and the full dev toolchain (`just dev`), you'll need to clone the repo. See [Local Dev](/docs/local-dev/overview) for the developer-focused setup with Go, Node, and `just`.
