# Changelog

## [0.6.3](https://github.com/openktree/open-knowledge-tree/compare/registry-v0.6.2...registry-v0.6.3) (2026-07-29)


### Bug Fixes

* **registry:** read role from DB on every request, not stale JWT claim ([20c680d](https://github.com/openktree/open-knowledge-tree/commit/20c680d3fe16ed367a7f23105f7ddb2085dcb309))
* **registry:** read role from DB on every request, not stale JWT claim ([4215084](https://github.com/openktree/open-knowledge-tree/commit/42150844d7a55699a94e2faf84f7e30649323968))

## [0.6.2](https://github.com/openktree/open-knowledge-tree/compare/registry-v0.6.1...registry-v0.6.2) (2026-07-29)


### Bug Fixes

* **registry:** make API tokens authenticate, prefix with okr_, render in copyable box ([93a70f2](https://github.com/openktree/open-knowledge-tree/commit/93a70f24d200c97d89ba2b88707079d9f4a7af43))
* **registry:** make API tokens authenticate, prefix with okr_, render in copyable box ([0e674ac](https://github.com/openktree/open-knowledge-tree/commit/0e674ac63461d6914a18718c4eda89c0e4d0c451))

## [0.6.1](https://github.com/openktree/open-knowledge-tree/compare/registry-v0.6.0...registry-v0.6.1) (2026-07-29)


### Bug Fixes

* **registry:** accept form-encoded POST on /ui/tokens create ([0722d39](https://github.com/openktree/open-knowledge-tree/commit/0722d39e32f53420d85e85a28bb0eed90ef839e2))
* **registry:** accept form-encoded POST on /ui/tokens create ([321ac4b](https://github.com/openktree/open-knowledge-tree/commit/321ac4b996f6037bc1e185059dd7aac1dc85e5ab))

## [0.6.0](https://github.com/openktree/open-knowledge-tree/compare/registry-v0.5.8...registry-v0.6.0) (2026-07-29)


### Features

* **registry:** add browser UI with sources/graphs/users/tokens pages ([67a54ef](https://github.com/openktree/open-knowledge-tree/commit/67a54ef0051d0495cf93b238677ac854d3a0abc7))
* **registry:** browser UI with sources/graphs/users/tokens pages ([d6b1346](https://github.com/openktree/open-knowledge-tree/commit/d6b1346d55504977a8c3cb2713aacbb54f12141a))


### Bug Fixes

* **registry:** accept JWT from `token` cookie in auth middleware ([8f53013](https://github.com/openktree/open-knowledge-tree/commit/8f53013a4e03b9125e37a4aded407b5cf398cf27))

## [0.5.8](https://github.com/openktree/open-knowledge-tree/compare/registry-v0.5.7...registry-v0.5.8) (2026-07-29)


### Bug Fixes

* **registry:** add admin endpoint to cleanup orphaned multipart uploads ([dca837e](https://github.com/openktree/open-knowledge-tree/commit/dca837ef3303874d65ef237d7490a6c59c56fd9f))

## [0.5.7](https://github.com/openktree/open-knowledge-tree/compare/registry-v0.5.6...registry-v0.5.7) (2026-07-28)


### Bug Fixes

* **registry:** backfill promptset_hash column on legacy SQLite volumes ([e66c1de](https://github.com/openktree/open-knowledge-tree/commit/e66c1deee39a3c67ef7afbc93cd6c7d2a8c3839a))

## [0.5.6](https://github.com/openktree/open-knowledge-tree/compare/registry-v0.5.5...registry-v0.5.6) (2026-07-28)


### Bug Fixes

* **registry:** persist promptset_hash + add enable_validation ([23e3ce2](https://github.com/openktree/open-knowledge-tree/commit/23e3ce22fca792216ed93f470d701f923830d784))

## [0.5.5](https://github.com/openktree/open-knowledge-tree/compare/registry-v0.5.4...registry-v0.5.5) (2026-07-28)


### Bug Fixes

* **registry:** remove WriteTimeout that 502'd long graph pushes ([e8c4b5b](https://github.com/openktree/open-knowledge-tree/commit/e8c4b5b44b0dca3729a5aa22fff2d4ef93184684))

## [0.5.4](https://github.com/openktree/open-knowledge-tree/compare/registry-v0.5.3...registry-v0.5.4) (2026-07-28)


### Bug Fixes

* **registry,api:** move graph metadata to headers + S3 multipart upload ([81c6a8d](https://github.com/openktree/open-knowledge-tree/commit/81c6a8d713ddba05e9f9d61e77aa707ce7e4934d))

## [0.5.3](https://github.com/openktree/open-knowledge-tree/compare/registry-v0.5.2...registry-v0.5.3) (2026-07-28)


### Bug Fixes

* **registry:** spool non-seekable graph bundle to a temp file before S3 PutObject ([95013ff](https://github.com/openktree/open-knowledge-tree/commit/95013ff0dee75ceef0d3818d2b2a67f7b5c1f573))

## [0.5.2](https://github.com/openktree/open-knowledge-tree/compare/registry-v0.5.1...registry-v0.5.2) (2026-07-27)


### Bug Fixes

* **registry:** stop json.Decode from pulling the whole bundle through gzip ([39d83b4](https://github.com/openktree/open-knowledge-tree/commit/39d83b4a4f94e84a9036a21ad0b7630f7074eefa))

## [0.5.1](https://github.com/openktree/open-knowledge-tree/compare/registry-v0.5.0...registry-v0.5.1) (2026-07-27)


### Bug Fixes

* **registry:** stream graph bundle metadata decode to avoid bufio buffer-full 400 ([f47023d](https://github.com/openktree/open-knowledge-tree/commit/f47023d34f550268ca6ce753ca8e9a0711c643e0))

## [0.5.0](https://github.com/openktree/open-knowledge-tree/compare/registry-v0.4.0...registry-v0.5.0) (2026-07-27)


### Features

* **graph:** streaming export + late-chunking baseline + kgqa experiment ([544c797](https://github.com/openktree/open-knowledge-tree/commit/544c7976bcb565ce683e980f61cedc300a4f8815))

## [0.4.0](https://github.com/openktree/open-knowledge-tree/compare/registry-v0.3.0...registry-v0.4.0) (2026-07-22)


### Features

* **synthesis:** retry synthesize_concept on LLM/write failures + per-concept resynthesize endpoint ([3fa0d28](https://github.com/openktree/open-knowledge-tree/commit/3fa0d28d34c5b2418846309f081fe04bca6d8253))

## [0.3.0](https://github.com/openktree/open-knowledge-tree/compare/registry-v0.2.0...registry-v0.3.0) (2026-07-19)


### Features

* promptsets system + registry search/cache adapters + content-type gate ([8d2d2db](https://github.com/openktree/open-knowledge-tree/commit/8d2d2dbad257fa6ac6b68d93a9c6c64c10531fbb))
* **registry:** bound heavy-op concurrency and enable autostop on prod ([95e2bb8](https://github.com/openktree/open-knowledge-tree/commit/95e2bb8683c19cfa474b16b620002f6a99317b69))


### Bug Fixes

* **registry:** scale prod VM to shared-cpu-2x / 4GB ([d8f8dea](https://github.com/openktree/open-knowledge-tree/commit/d8f8dea2fc91daeb57aa46be78df9e676516b752))
* **registry:** surface decode errors and drop 30s ReadTimeout ([a412c74](https://github.com/openktree/open-knowledge-tree/commit/a412c74c404cba61593b8086232d342996b6723a))

## [0.2.0](https://github.com/openktree/open-knowledge-tree/compare/registry-v0.1.0...registry-v0.2.0) (2026-07-18)


### Features

* **registry:** add filesystem storage backend for VM-only Fly dev deploy ([53cbdd0](https://github.com/openktree/open-knowledge-tree/commit/53cbdd034c75f1947b4cd908f06bfa7894a4c949))


### Bug Fixes

* **registry:** replace minio-go with aws-sdk-go-v2 for R2 compatibility ([b903c4f](https://github.com/openktree/open-knowledge-tree/commit/b903c4f7daea823421d506eb4af9f01f817533c9))
* **registry:** restore [build] image line in fly.toml for pipeline deploys ([990d4bb](https://github.com/openktree/open-knowledge-tree/commit/990d4bbdfbaff10263b71358cada1634ea643bd6))

## [0.1.0] (2026-07-17)


### Features

* **registry:** add filesystem storage backend for VM-only Fly dev deploy ([53cbdd0](https://github.com/openktree/open-knowledge-tree/commit/53cbdd034c75f1947b4cd908f06bfa7894a4c949))

## Changelog (registry)

Releases are tagged `registry-v<semver>` and published as `ghcr.io/openktree/registry:<version>`.
