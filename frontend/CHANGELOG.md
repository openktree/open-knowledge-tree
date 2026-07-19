# Changelog

## [1.2.0](https://github.com/openktree/open-knowledge-tree/compare/frontend-v1.1.0...frontend-v1.2.0) (2026-07-19)


### Features

* **promptset:** split registry-compatibility hash from catalog hash ([8f1d2af](https://github.com/openktree/open-knowledge-tree/commit/8f1d2af510e3fe4963c3a81e1e3f822586411c64))
* **registry:** per-repo contributor identity for registry attribution ([45f28ae](https://github.com/openktree/open-knowledge-tree/commit/45f28aebe4cdeb6f350930f4115138c189052675))

## [1.1.0](https://github.com/openktree/open-knowledge-tree/compare/frontend-v1.0.0...frontend-v1.1.0) (2026-07-19)


### Features

* promptsets system + registry search/cache adapters + content-type gate ([8d2d2db](https://github.com/openktree/open-knowledge-tree/commit/8d2d2dbad257fa6ac6b68d93a9c6c64c10531fbb))
* **ui:** fetch remote decomp directly from R2 via presigned URL ([6dd23a7](https://github.com/openktree/open-knowledge-tree/commit/6dd23a762b9ca28f6ac2065ff7404cc9cc5c3aab))


### Bug Fixes

* **ui:** route decomp fetch through backend, skip direct R2 fetch ([bccb39a](https://github.com/openktree/open-knowledge-tree/commit/bccb39af65e21de719495a4c39b88a1452b33a9a))

## 1.0.0 (2026-07-18)


### Features

* **providers:** per-provider host failure cards on Providers page ([fc6e2ce](https://github.com/openktree/open-knowledge-tree/commit/fc6e2ce4cefb8628da63e11801ef162bbb27315b))


### Bug Fixes

* **registry:** replace minio-go with aws-sdk-go-v2 for R2 compatibility ([b903c4f](https://github.com/openktree/open-knowledge-tree/commit/b903c4f7daea823421d506eb4af9f01f817533c9))

## Changelog (frontend)

Releases are tagged `frontend-v<semver>` and published as `ghcr.io/openktree/frontend:<version>`.
