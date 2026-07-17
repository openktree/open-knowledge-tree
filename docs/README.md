# Open Knowledge Tree — Docs

The Docusaurus documentation site for Open Knowledge Tree.

## Quick start

```bash
# Dev server with hot reload (localhost:3001)
just docs

# Or with npm directly:
cd docs && npm install && npm run start -- --port 3001

# Production build
just docs-build

# Serve via Docker (localhost:3002)
just docs-serve
```

## Structure

- `src/pages/index.tsx` — landing page (hero + pipeline diagram + feature cards)
- `src/components/PipelineDiagram.tsx` — 7-stage pipeline visual
- `docs/` — the doc pages:
  - `intro.md` — introduction
  - `core-flow/` — the 7-stage ingestion pipeline
  - `mcp/` — MCP tools reference
  - `api/` — REST API reference
  - `local-dev/` — local development guide
  - `architecture/` — architecture deep-dive

## Configuration

The site config is in `docusaurus.config.js`. The sidebar tree is in `sidebars.js`. Theme overrides are in `src/css/custom.css`.