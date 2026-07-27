# Changelog

## [0.4.0](https://github.com/openktree/open-knowledge-tree/compare/frontend-v0.3.0...frontend-v0.4.0) (2026-07-27)


### Features

* **api:** hybrid lexical retrieval for annotate_report + dedup improvements + WIP ([74b24a6](https://github.com/openktree/open-knowledge-tree/commit/74b24a611afc39d369f58bf046f00c0f5c6047d2))
* **audit:** expand audit coverage across handlers + bootstrap; docs + compose updates ([fb96078](https://github.com/openktree/open-knowledge-tree/commit/fb96078772af2913e44de81776b82dea567b645a))
* **citations:** open inline fact/concept modal instead of navigating away ([2b18ea2](https://github.com/openktree/open-knowledge-tree/commit/2b18ea235910a730f3d4338bdfce768fa3a7214d))
* **cited-copy:** explain sections, keep posture on dedup, group by source ([646738b](https://github.com/openktree/open-knowledge-tree/commit/646738bd01128f85aeedf395e3a565f68450824e))
* **cited-copy:** include inline-cited facts in the cited-copy annex ([62b85e6](https://github.com/openktree/open-knowledge-tree/commit/62b85e64797ff0833a46cfec2e010099e2a5841c))
* **concepts:** concept sources endpoint + MCP tool + UI provenance, and fact-summary curriculum ([1e68182](https://github.com/openktree/open-knowledge-tree/commit/1e6818272256a4800e0f056e4a5bb80c5138a86c))
* **graph:** embed source images + optional PDFs in graph bundles ([7df06b8](https://github.com/openktree/open-knowledge-tree/commit/7df06b83d70465bc432598351379e932ede993f4))
* **graph:** streaming export + late-chunking baseline + kgqa experiment ([544c797](https://github.com/openktree/open-knowledge-tree/commit/544c7976bcb565ce683e980f61cedc300a4f8815))
* **graph:** synchronous download endpoint for file-based graph export ([1366156](https://github.com/openktree/open-knowledge-tree/commit/13661567fba75fad0950eee42f0a3a678c539df1))
* **graph:** upload bundle UI in Shared Graphs page ([a663a14](https://github.com/openktree/open-knowledge-tree/commit/a663a1479f59f0e70da3b323ecf3ff4125e5c4ca))
* **search:** hybrid lexical+TSV retrieval with RRF, plus audit/API keys/claims infra ([59e1698](https://github.com/openktree/open-knowledge-tree/commit/59e1698cb7905f5c45ccef7854c3e8220fbe27bc))
* **synthesis:** retry synthesize_concept on LLM/write failures + per-concept resynthesize endpoint ([3fa0d28](https://github.com/openktree/open-knowledge-tree/commit/3fa0d28d34c5b2418846309f081fe04bca6d8253))


### Bug Fixes

* **citations:** route report/source citations + resolve kinds by lookup ([28aaf98](https://github.com/openktree/open-knowledge-tree/commit/28aaf9893ff1cdcfe0b73167027e3c08ec5cc828))
* **citations:** unwrap fact from getFact envelope in CitationModal ([c3652c4](https://github.com/openktree/open-knowledge-tree/commit/c3652c4118a9e589df77c5152e1e1f82fc053e16))
* **cited-copy:** direct cites first, dedup from auto-matched, fetch all inline ([241de58](https://github.com/openktree/open-knowledge-tree/commit/241de5864aae6fe666f5dcfd52033ae0d295779d))
* **cited-copy:** replace inline links with [D{M}] markers; record fetch errors ([cb011e7](https://github.com/openktree/open-knowledge-tree/commit/cb011e793c58aa881032af9240008769909595fa))
* **graph:** raise server write_timeout + vite proxy timeout for graph download ([a15168a](https://github.com/openktree/open-knowledge-tree/commit/a15168a23d699e37f1cd1e88b22cfe25f7bac722))
* **graph:** treat props.graph as value not function in import dialog ([191a3fd](https://github.com/openktree/open-knowledge-tree/commit/191a3fd9b996bd381aff478089e57f0941758d21))
* **modals:** show error + id on 404 instead of freezing; add source URLs ([3db8cd6](https://github.com/openktree/open-knowledge-tree/commit/3db8cd6d15b4c7c4b63876a231d6b5c458f3f89a))

## [0.3.0](https://github.com/openktree/open-knowledge-tree/compare/frontend-v0.2.0...frontend-v0.3.0) (2026-07-19)


### Features

* **promptset:** split registry-compatibility hash from catalog hash ([8f1d2af](https://github.com/openktree/open-knowledge-tree/commit/8f1d2af510e3fe4963c3a81e1e3f822586411c64))
* **registry:** per-repo contributor identity for registry attribution ([45f28ae](https://github.com/openktree/open-knowledge-tree/commit/45f28aebe4cdeb6f350930f4115138c189052675))

## [0.2.0](https://github.com/openktree/open-knowledge-tree/compare/frontend-v0.1.0...frontend-v0.2.0) (2026-07-19)


### Features

* promptsets system + registry search/cache adapters + content-type gate ([8d2d2db](https://github.com/openktree/open-knowledge-tree/commit/8d2d2dbad257fa6ac6b68d93a9c6c64c10531fbb))
* **ui:** fetch remote decomp directly from R2 via presigned URL ([6dd23a7](https://github.com/openktree/open-knowledge-tree/commit/6dd23a762b9ca28f6ac2065ff7404cc9cc5c3aab))


### Bug Fixes

* **ui:** route decomp fetch through backend, skip direct R2 fetch ([bccb39a](https://github.com/openktree/open-knowledge-tree/commit/bccb39af65e21de719495a4c39b88a1452b33a9a))

## [0.1.0] (2026-07-18)


### Features

* **providers:** per-provider host failure cards on Providers page ([fc6e2ce](https://github.com/openktree/open-knowledge-tree/commit/fc6e2ce4cefb8628da63e11801ef162bbb27315b))


### Bug Fixes

* **registry:** replace minio-go with aws-sdk-go-v2 for R2 compatibility ([b903c4f](https://github.com/openktree/open-knowledge-tree/commit/b903c4f7daea823421d506eb4af9f01f817533c9))

## Changelog (frontend)

Releases are tagged `frontend-v<semver>` and published as `ghcr.io/openktree/frontend:<version>`.
