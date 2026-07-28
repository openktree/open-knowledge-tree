# Changelog

## [0.5.2](https://github.com/openktree/open-knowledge-tree/compare/api-v0.5.1...api-v0.5.2) (2026-07-28)


### Bug Fixes

* **api:** chain deduplicate_facts when embed_facts no-ops (registry pull fix) ([637bf5c](https://github.com/openktree/open-knowledge-tree/commit/637bf5ce786e31d6430e3e6b831971002dbd0b54))

## [0.5.1](https://github.com/openktree/open-knowledge-tree/compare/api-v0.5.0...api-v0.5.1) (2026-07-28)


### Bug Fixes

* **registry,api:** move graph metadata to headers + S3 multipart upload ([81c6a8d](https://github.com/openktree/open-knowledge-tree/commit/81c6a8d713ddba05e9f9d61e77aa707ce7e4934d))

## [0.5.0](https://github.com/openktree/open-knowledge-tree/compare/api-v0.4.3...api-v0.5.0) (2026-07-27)


### Features

* **api:** enable all tasks by default + import registry embeddings + guard contribute_all ([2896b16](https://github.com/openktree/open-knowledge-tree/commit/2896b16f2089bfed0ce4544c74dfcd7f372aca9a))

## [0.4.3](https://github.com/openktree/open-knowledge-tree/compare/api-v0.4.2...api-v0.4.3) (2026-07-27)


### Bug Fixes

* **api:** import registry concepts verbatim when context label matches local vocab ([9b55a15](https://github.com/openktree/open-knowledge-tree/commit/9b55a1525c0af4ee21e8502e6841fb3c931c7b85))

## [0.4.2](https://github.com/openktree/open-knowledge-tree/compare/api-v0.4.1...api-v0.4.2) (2026-07-27)


### Bug Fixes

* **api:** provider-agnostic model matching + auto-whitelist repo's own fact model ([8553f69](https://github.com/openktree/open-knowledge-tree/commit/8553f694558b29c4af07c2d83a537884e2e0969f))

## [0.4.1](https://github.com/openktree/open-knowledge-tree/compare/api-v0.4.0...api-v0.4.1) (2026-07-27)


### Bug Fixes

* **api:** honor allowed_models + per-repo override on sync registry pull ([729023b](https://github.com/openktree/open-knowledge-tree/commit/729023bba5238d2883fa589acd33b34f1f1c828f))

## [0.4.0](https://github.com/openktree/open-knowledge-tree/compare/api-v0.3.0...api-v0.4.0) (2026-07-27)


### Features

* **api:** hybrid lexical retrieval for annotate_report + dedup improvements + WIP ([74b24a6](https://github.com/openktree/open-knowledge-tree/commit/74b24a611afc39d369f58bf046f00c0f5c6047d2))
* **audit:** expand audit coverage across handlers + bootstrap; docs + compose updates ([fb96078](https://github.com/openktree/open-knowledge-tree/commit/fb96078772af2913e44de81776b82dea567b645a))
* **concepts:** concept sources endpoint + MCP tool + UI provenance, and fact-summary curriculum ([1e68182](https://github.com/openktree/open-knowledge-tree/commit/1e6818272256a4800e0f056e4a5bb80c5138a86c))
* **graph:** embed source images + optional PDFs in graph bundles ([7df06b8](https://github.com/openktree/open-knowledge-tree/commit/7df06b83d70465bc432598351379e932ede993f4))
* **graph:** streaming export + late-chunking baseline + kgqa experiment ([544c797](https://github.com/openktree/open-knowledge-tree/commit/544c7976bcb565ce683e980f61cedc300a4f8815))
* **graph:** synchronous download endpoint for file-based graph export ([1366156](https://github.com/openktree/open-knowledge-tree/commit/13661567fba75fad0950eee42f0a3a678c539df1))
* **search:** hybrid lexical+TSV retrieval with RRF, plus audit/API keys/claims infra ([59e1698](https://github.com/openktree/open-knowledge-tree/commit/59e1698cb7905f5c45ccef7854c3e8220fbe27bc))
* **synthesis:** retry synthesize_concept on LLM/write failures + per-concept resynthesize endpoint ([3fa0d28](https://github.com/openktree/open-knowledge-tree/commit/3fa0d28d34c5b2418846309f081fe04bca6d8253))


### Bug Fixes

* **audit:** pass nil repoID for system-scope audit row assertions ([ba1a858](https://github.com/openktree/open-knowledge-tree/commit/ba1a8581de7c5970022368d150fd078d4391ee58))
* **citations:** route report/source citations + resolve kinds by lookup ([28aaf98](https://github.com/openktree/open-knowledge-tree/commit/28aaf9893ff1cdcfe0b73167027e3c08ec5cc828))
* **graph:** enqueue concept_groups recompute + relations refresh after import ([92e90ee](https://github.com/openktree/open-knowledge-tree/commit/92e90ee60e1c5a84a71d0a4f32f752a87ef54689))
* **graph:** raise server write_timeout + vite proxy timeout for graph download ([a15168a](https://github.com/openktree/open-knowledge-tree/commit/a15168a23d699e37f1cd1e88b22cfe25f7bac722))
* **graph:** raise upload max bytes to 20GB + ParseMultipartForm memory to 1GB ([c77149d](https://github.com/openktree/open-knowledge-tree/commit/c77149d177edb140562e10f369c96e9ddcb481f1))
* **graph:** shared-graphs browse endpoints don't need repo context ([cc846cd](https://github.com/openktree/open-knowledge-tree/commit/cc846cd1978bf6485ed53d247717e2e0d59f9960))
* **graph:** stream download to avoid OOM on large repos with images+PDFs ([5dba816](https://github.com/openktree/open-knowledge-tree/commit/5dba81605eab9746cf8eeb3741dd8cf747c4ac0e))
* **graph:** use correct tier value for new repo creation ([971ace3](https://github.com/openktree/open-knowledge-tree/commit/971ace37ecb36c7e3f82e3c66d249f10dfd36b25))
* **summarize:** propagate enqueue errors to River instead of swallowing ([7ccebd5](https://github.com/openktree/open-knowledge-tree/commit/7ccebd5f8ab1ffaab784f12f135afa0ef0e785c1))

## [0.3.0](https://github.com/openktree/open-knowledge-tree/compare/api-v0.2.0...api-v0.3.0) (2026-07-19)


### Features

* **promptset:** split registry-compatibility hash from catalog hash ([8f1d2af](https://github.com/openktree/open-knowledge-tree/commit/8f1d2af510e3fe4963c3a81e1e3f822586411c64))
* **registry:** per-repo contributor identity for registry attribution ([45f28ae](https://github.com/openktree/open-knowledge-tree/commit/45f28aebe4cdeb6f350930f4115138c189052675))

## [0.2.0](https://github.com/openktree/open-knowledge-tree/compare/api-v0.1.0...api-v0.2.0) (2026-07-19)


### Features

* **backend:** fetch remote decomp via presigned URL, skip registry re-marshal ([c9b2c1d](https://github.com/openktree/open-knowledge-tree/commit/c9b2c1d748e154b5fe22a034d2481a5a59acf9c8))
* **backend:** route cache-hit pulls through presigned URL too ([38d3c35](https://github.com/openktree/open-knowledge-tree/commit/38d3c35ab3539dfe06a17f88b7f32bfb7cad6f7c))
* **bootstrap:** auto-promote first registered user to sysadmin ([49612fd](https://github.com/openktree/open-knowledge-tree/commit/49612fd947f8234cf5550d45f6071742a0009883))
* promptsets system + registry search/cache adapters + content-type gate ([8d2d2db](https://github.com/openktree/open-knowledge-tree/commit/8d2d2dbad257fa6ac6b68d93a9c6c64c10531fbb))


### Bug Fixes

* **api:** pin runtime Alpine to 3.24 to match builder MuPDF SONAME ([f6418ae](https://github.com/openktree/open-knowledge-tree/commit/f6418aec0b001dbbbde0c956de4a310cb86c7b71))

## [0.1.0] (2026-07-18)


### Features

* **providers:** per-provider host failure cards on Providers page ([fc6e2ce](https://github.com/openktree/open-knowledge-tree/commit/fc6e2ce4cefb8628da63e11801ef162bbb27315b))


### Bug Fixes

* **registry:** replace minio-go with aws-sdk-go-v2 for R2 compatibility ([b903c4f](https://github.com/openktree/open-knowledge-tree/commit/b903c4f7daea823421d506eb4af9f01f817533c9))

## Changelog (api)

Releases are tagged `api-v<semver>` and published as `ghcr.io/openktree/api:<version>`.
