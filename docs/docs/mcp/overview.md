---
id: overview
sidebar_position: 0
title: MCP Overview
---

# MCP Overview

OKT exposes 18 tools via the [Model Context Protocol](https://modelcontextprotocol.io/) (MCP), allowing an AI agent to fetch sources, search facts and concepts, track ingestion, and create annotated reports — all over OAuth 2.1 bearer auth.

## Endpoint

The MCP server is mounted at `POST /api/v1/mcp` and wrapped with the `OAuthBearer` middleware. It uses the `github.com/mark3labs/mcp-go` library. See `backend/internal/api/handler/mcp.go:192`.

## Auth

MCP calls authenticate via OAuth 2.1 access tokens (HS256 JWTs signed with `cfg.Auth.JWTSecret`). The token is obtained via the standard OAuth 2.1 authorize/token flow (see [REST API > Auth](/docs/api/auth)) and passed as a `Bearer` token in the `Authorization` header.

Two well-known documents are served at the router root:
- `/.well-known/oauth-authorization-server` — the authorization server metadata.
- `/.well-known/oauth-protected-resource` — the protected resource metadata.

## Repository scoping

Most tools take a `repository` argument that accepts a UUID or a slug. The tool resolves it the same way the REST API's `/{repoID}` routes do — through the dbpool registry. The token's subject (user) must have read or write permission on the repository for the tool to succeed.

## Tool categories

| Category | Tools |
|----------|-------|
| Discovery | `getRepositories`, `listSearchProviders` |
| Search | `searchSources`, `searchFacts`, `searchConcepts` |
| Fact detail | `getFact` |
| Concept detail | `getConcept`, `getConceptSummaries`, `getRelatedConcepts` |
| Ingestion | `fetchAndProcessSource`, `getSourceTasks` |
| Investigations | `getInvestigation`, `createInvestigation`, `addInvestigationSource` |
| Reports | `createReport`, `getReport`, `listReports`, `getReportTasks` |

## The typical agent workflow

1. `getRepositories` — find the repository to work in.
2. `createInvestigation` — create a collection for a topic.
3. `searchSources` — find candidate URLs (web or academic).
4. `fetchAndProcessSource` (with `investigationId`) — fetch + organize in one call.
5. `getSourceTasks` (poll) — wait for the 7-stage pipeline to drain.
6. `searchFacts` / `searchConcepts` / `getConcept` — query the knowledge graph.
7. `createReport` — write a report with autofact annotation.

Ready to wire up a client? See [Getting Started with MCP](/docs/mcp/getting-started) for per-client setup (Claude Desktop, Cursor, VS Code, Claude Code, MCP Inspector) and the manual OAuth flow.

See the full tool reference at [MCP Tools Reference](/docs/mcp/tools).