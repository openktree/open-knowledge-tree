---
id: getting-started
sidebar_position: 0
title: Getting Started with MCP
---

# Getting Started with MCP

The OKT MCP server is mounted at `POST /api/v1/mcp` and is protected by OAuth 2.1 with PKCE. This page walks through everything you need to connect a client, get a token, and run your first tool call — followed by per-client snippets for the most common MCP clients.

If you just want the list of tools and their arguments, skip to [MCP Tools Reference](/docs/mcp/tools). For the high-level architecture and auth model, see [MCP Overview](/docs/mcp/overview).

## Prerequisites

- A running OKT instance. For local dev, `just dev` boots the full stack on `http://localhost:8080` (see [Local Dev](/docs/local-dev/overview)).
- A user account on that instance. Register one at `POST /api/v1/auth/register` or via the frontend.
- Your user must hold a role that grants permission on the repository you want to query (see [Architecture > RBAC](/docs/architecture/rbac)). The bootstrap admin shortcut is `just bootstrap-admin you@example.com`.

## How an MCP client authenticates

OKT implements the standard OAuth 2.1 + RFC 7591 + RFC 8414 + RFC 9728 flow that modern MCP clients (Claude Desktop, Cursor, the MCP Inspector, etc.) speak natively. You normally do **not** need to do this by hand — a compliant client self-registers and runs the authorize/token flow on first connect. The manual flow below is here so you understand what the client is doing, and so you can debug when it breaks.

Two well-known documents drive the auto-discovery:

| URL | RFC | What it tells the client |
|-----|-----|--------------------------|
| `/.well-known/oauth-authorization-server` | 8414 | `authorization_endpoint`, `token_endpoint`, `registration_endpoint`, supported PKCE methods (`S256` only), scopes (`mcp`). |
| `/.well-known/oauth-protected-resource` | 9728 | `resource` = MCP endpoint URL, `authorization_servers` = the issuer. |

When the client hits `POST /api/v1/mcp` without a bearer token, the server replies `401` with a `WWW-Authenticate: Bearer resource_metadata_url=...` header pointing at the protected-resource document. The client follows that to discover the authorization server, registers a client, runs the authorize + token PKCE flow, and retries the MCP call with the access token.

### Manual flow (for debugging or scripts)

The e2e tests in `backend/e2e/oauth_test.go` exercise the full flow; this is a condensed version.

1. **Register a client** (RFC 7591):

   ```bash
   curl -X POST http://localhost:8080/api/v1/oauth/register \
     -H 'Content-Type: application/json' \
     -d '{
       "redirect_uris": ["http://127.0.0.1:8765/callback"],
       "client_name": "my-script"
     }'
   ```

   The response includes a `client_id` (an opaque string). Public clients only — no `client_secret`; PKCE is the confidentiality mechanism.

2. **Generate a PKCE pair.** The verifier is a random 43+ char URL-safe string; the challenge is `base64url(sha256(verifier))` without padding.

   ```bash
   VERIFIER="$(openssl rand -base64 32 | tr -d '/+=' | head -c 43)"
   CHALLENGE="$(printf '%s' "$VERIFIER" | openssl dgst -binary -sha256 | base64 | tr -d '/+=\n')"
   ```

3. **Authorize** (interactive browser flow). Open this URL in a browser logged in to OKT:

   ```
   http://localhost:8080/api/v1/oauth/authorize?response_type=code&client_id=<CLIENT_ID>&redirect_uri=http://127.0.0.1:8765/callback&code_challenge=<CHALLENGE>&code_challenge_method=S256&scope=mcp&state=<STATE>
   ```

   After login + consent, OKT 302s to `http://127.0.0.1:8765/callback?code=<CODE>&state=<STATE>`. Capture the `code` (it is single-use and short-lived).

4. **Exchange the code for tokens:**

   ```bash
   curl -X POST http://localhost:8080/api/v1/oauth/token \
     -d "grant_type=authorization_code" \
     -d "client_id=<CLIENT_ID>" \
     -d "code=<CODE>" \
     -d "redirect_uri=http://127.0.0.1:8765/callback" \
     -d "code_verifier=<VERIFIER>"
   ```

   Returns `{access_token, refresh_token, token_type:"Bearer", expires_in}`. The access token is an HS256 JWT; pass it as `Authorization: Bearer <access_token>` on every MCP call.

5. **Call an MCP tool.** MCP is JSON-RPC over HTTP; the `tools/call` method invokes a tool:

   ```bash
   curl -X POST http://localhost:8080/api/v1/mcp \
     -H 'Authorization: Bearer <ACCESS_TOKEN>' \
     -H 'Content-Type: application/json' \
     -H 'Accept: application/json, text/event-stream' \
     -d '{
       "jsonrpc": "2.0",
       "id": 1,
       "method": "tools/call",
       "params": {
         "name": "getRepositories",
         "arguments": {}
       }
     }'
   ```

   The server may respond with `application/json` or `text/event-stream` (one event per chunk). Stateless mode means you do not need to send an `initialize` handshake first.

## Connecting common clients

All clients below auto-discover via the two well-known URLs, so in most cases you only need to give the client the MCP endpoint URL and log in through the browser the client opens.

### Claude Desktop

Add an entry to `claude_desktop_config.json` (Settings → Developer → Edit Config). Claude Desktop speaks the Streamable HTTP transport and will self-register an OAuth client on first connect.

```jsonc
{
  "mcpServers": {
    "okt": {
      "type": "http",
      "url": "http://localhost:8080/api/v1/mcp"
    }
  }
}
```

Restart Claude Desktop. The first `tools/call` triggers a 401 → Claude opens a browser to the OKT authorize endpoint, you log in + consent, and Claude caches the access token for subsequent calls.

### Cursor

Open **Settings → MCP → Add new MCP server** and configure:

- **Type:** `http`
- **URL:** `http://localhost:8080/api/v1/mcp`
- **Name:** `okt`

Cursor reads the well-known metadata and runs the same OAuth flow as Claude Desktop. Once connected, the 18 OKT tools are available as `okt__<tool_name>` (e.g. `okt__searchFacts`, `okt__getConcept`) in the agent tool picker.

### VS Code (Copilot Chat) / GitHub Copilot

Edit `.vscode/mcp.json` in your workspace (or your user settings):

```jsonc
{
  "servers": {
    "okt": {
      "type": "http",
      "url": "http://localhost:8080/api/v1/mcp"
    }
  }
}
```

Copilot discovers the tools via the well-known metadata and runs the OAuth flow the first time a tool is invoked in a chat. You can also enable OKT in **Chat → Tools** to let the agent call the tools autonomously.

### Claude Code (CLI)

Run once to add the server to your project's `.mcp.json`:

```bash
claude mcp add --transport http okt http://localhost:8080/api/v1/mcp
```

Then `claude` will pick the server up and prompt you through the OAuth flow on first use.

### MCP Inspector

The official Inspector (`@modelcontextprotocol/inspector`) is the fastest way to test a tool by hand:

```bash
npx @modelcontextprotocol/inspector
```

In the UI, set **Transport Type** to `Streamable HTTP` and **URL** to `http://localhost:8080/api/v1/mcp`. Click Connect; the Inspector runs the OAuth flow in a popup, lists the 18 tools, and lets you call each with a JSON args editor. This is the easiest way to verify a tool's argument shape before wiring it into a client.

## Verifying the connection

Once a client is connected, call `getRepositories` (it takes no arguments). It should return the repositories your user can access:

```json
[
  { "id": "...", "name": "My repo", "slug": "my-repo", "tier": "shared", "roles": ["reader"] }
]
```

If you get an empty array, your user has no role on any repository — ask a sysadmin to assign one, or create a repository via the REST API. If you get a 403-style tool error, your role lacks the `repositories:read` permission (see [Architecture > RBAC](/docs/architecture/rbac)).

From here, follow the typical agent workflow in [MCP Overview](/docs/mcp/overview#the-typical-agent-workflow), or jump to the full [MCP Tools Reference](/docs/mcp/tools).

## Troubleshooting

- **`401 Unauthorized` on the MCP endpoint** — the access token is missing, expired, or signed with the wrong secret. Re-run the OAuth flow; check `cfg.Auth.JWTSecret` matches what signed the token.
- **`403` / tool error "permission denied"** — your user's role on the repository doesn't grant the required action. Use `just bootstrap-admin you@example.com` for a system admin (gives `*/*`), or assign a repository-scoped role via the admin API.
- **`invalid_request` from `/oauth/authorize`** — missing or non-S256 `code_challenge`. OAuth 2.1 mandates PKCE with S256; "plain" is not supported.
- **Authorize flow never reaches consent** — the `redirect_uri` in the authorize request is not in the registered client's `redirect_uris`. Re-register the client with the correct URI.
- **Client says "no tools found"** — confirm the well-known URLs respond (`curl http://localhost:8080/.well-known/oauth-authorization-server`). Some clients cache discovery; restart the client after a server change.

More in [Local Dev > Troubleshooting](/docs/local-dev/troubleshooting).