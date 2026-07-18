---
id: tutorial-mcp-tool
sidebar_position: 3
title: Creating an MCP Tool
---

# Tutorial: Creating an MCP Tool

MCP tools let AI agents interact with OKT — fetching sources, searching facts, creating reports, and more. This tutorial shows how to add a new tool.

## How MCP tools work

Tools are registered in `backend/internal/api/handler/mcp.go` via `m.mcpServer.AddTool(definition, handlerFunc)`. Each tool has:

1. A **name** and **description** (shown to the agent)
2. An **input schema** (JSON Schema for the parameters)
3. A **handler function** that receives the params and returns a result

The MCP library is [mark3labs/mcp-go](https://github.com/mark3labs/mcp-go).

## Step 1: Add the tool definition

In `backend/internal/api/handler/mcp.go`, find the `registerTools()` method and add:

```go
func (m *MCP) registerTools() {
    // ... existing tools ...

    // Search facts by concept name
    m.mcpServer.AddTool(
        mcp.NewTool("searchByConcept",
            mcp.WithDescription("Search for facts linked to a specific concept name in a repository"),
            mcp.WithString("repository",
                mcp.Required(),
                mcp.Description("Repository UUID or slug"),
            ),
            mcp.WithString("concept",
                mcp.Required(),
                mcp.Description("Concept canonical name to search for"),
            ),
            mcp.WithNumber("limit",
                mcp.Description("Maximum facts to return (default 10)"),
            ),
        ),
        m.handleSearchByConcept,
    )
}
```

## Step 2: Implement the handler

Add the handler method on `*MCP`:

```go
func (m *MCP) handleSearchByConcept(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
    // 1. Extract parameters
    repoIDOrSlug, ok := req.Params.Arguments["repository"].(string)
    if !ok {
        return mcp.NewToolResultError("repository is required"), nil
    }
    concept, ok := req.Params.Arguments["concept"].(string)
    if !ok {
        return mcp.NewToolResultError("concept is required"), nil
    }
    limit := 10
    if v, ok := req.Params.Arguments["limit"].(float64); ok && v > 0 {
        limit = int(v)
    }

    // 2. Resolve the repository
    repoID, pool, err := m.resolveRepoPool(ctx, repoIDOrSlug)
    if err != nil {
        return mcp.NewToolResultError(fmt.Sprintf("repository not found: %v", err)), nil
    }
    q := store.New(pool)

    // 3. Query facts linked to the concept
    facts, err := q.SearchFactsByConcept(ctx, store.SearchFactsByConceptParams{
        RepositoryID:    repoID,
        CanonicalName:   concept,
        Limit:           int32(limit),
    })
    if err != nil {
        return mcp.NewToolResultError(fmt.Sprintf("query failed: %v", err)), nil
    }

    // 4. Format and return
    items := make([]map[string]interface{}, 0, len(facts))
    for _, f := range facts {
        items = append(items, map[string]interface{}{
            "id":     f.ID.String(),
            "text":   f.Text,
            "status": f.Status,
        })
    }

    return mcp.NewToolResultText(fmt.Sprintf(`{"concept":"%s","facts":%s}`, concept, marshalJSON(items))), nil
}
```

## Step 3: Wire dependencies (if needed)

If your tool needs access to the task enqueuer, search providers, or other services, use the existing setter methods:

```go
m.taskEnqueuer  // for enqueueing background jobs
m.searchProviders  // for searching sources
```

If you need a new dependency, add a `Set*` method following the existing pattern:

```go
func (m *MCP) SetMyService(s MyService) { m.myService = s }
```

Then call it from `cmd/app/api.go` after `NewMCP(...)`.

## Step 4: Test it

### Unit test

Create `backend/internal/api/handler/mcp_test.go`:

```go
package handler_test

import (
    "testing"

    "github.com/mark3labs/mcp-go/mcp"
)

func TestSearchByConcept_ToolRegistered(t *testing.T) {
    // Verify the tool is registered on the MCP server
    // by listing tools and checking for "searchByConcept"
}
```

### MCP Inspector

Use the [MCP Inspector](https://github.com/modelcontextprotocol/inspector) to test interactively:

```bash
npx @modelcontextprotocol/inspector
```

Point it at `http://localhost:8080/api/v1/mcp` and authenticate via OAuth. You'll see your new tool in the tool list.

### Manual test

```bash
# Authenticate first, then call the tool via the MCP endpoint
curl -X POST http://localhost:8080/api/v1/mcp \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"tools/call","params":{"name":"searchByConcept","arguments":{"repository":"my-repo","concept":"CRISPR"}},"id":1}'
```

## Summary

| File | Change |
|------|--------|
| `backend/internal/api/handler/mcp.go` | Add tool definition in `registerTools()` + handler method |
| `backend/cmd/app/api.go` | Wire new dependencies via setter (if needed) |
| `backend/internal/api/handler/mcp_test.go` | Unit test (optional) |

## Tips

- Keep tool descriptions short and action-oriented — agents read them to decide which tool to call.
- Use `mcp.NewToolResultError(msg)` for user-facing errors (they don't crash the agent).
- Return structured JSON when the output is complex — agents parse it better than prose.
- The `repository` parameter is nearly universal — resolve it early with `m.resolveRepoPool()`.
