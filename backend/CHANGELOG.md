# Changelog

## [1.1.0](https://github.com/openktree/open-knowledge-tree/compare/api-v1.0.0...api-v1.1.0) (2026-07-19)


### Features

* **backend:** fetch remote decomp via presigned URL, skip registry re-marshal ([c9b2c1d](https://github.com/openktree/open-knowledge-tree/commit/c9b2c1d748e154b5fe22a034d2481a5a59acf9c8))
* **backend:** route cache-hit pulls through presigned URL too ([38d3c35](https://github.com/openktree/open-knowledge-tree/commit/38d3c35ab3539dfe06a17f88b7f32bfb7cad6f7c))
* **bootstrap:** auto-promote first registered user to sysadmin ([49612fd](https://github.com/openktree/open-knowledge-tree/commit/49612fd947f8234cf5550d45f6071742a0009883))
* promptsets system + registry search/cache adapters + content-type gate ([8d2d2db](https://github.com/openktree/open-knowledge-tree/commit/8d2d2dbad257fa6ac6b68d93a9c6c64c10531fbb))


### Bug Fixes

* **api:** pin runtime Alpine to 3.24 to match builder MuPDF SONAME ([f6418ae](https://github.com/openktree/open-knowledge-tree/commit/f6418aec0b001dbbbde0c956de4a310cb86c7b71))

## 1.0.0 (2026-07-18)


### Features

* **providers:** per-provider host failure cards on Providers page ([fc6e2ce](https://github.com/openktree/open-knowledge-tree/commit/fc6e2ce4cefb8628da63e11801ef162bbb27315b))


### Bug Fixes

* **registry:** replace minio-go with aws-sdk-go-v2 for R2 compatibility ([b903c4f](https://github.com/openktree/open-knowledge-tree/commit/b903c4f7daea823421d506eb4af9f01f817533c9))

## Changelog (api)

Releases are tagged `api-v<semver>` and published as `ghcr.io/openktree/api:<version>`.
