# Changelog

## [1.2.0](https://github.com/openktree/open-knowledge-tree/compare/registry-v1.1.0...registry-v1.2.0) (2026-07-19)


### Features

* promptsets system + registry search/cache adapters + content-type gate ([8d2d2db](https://github.com/openktree/open-knowledge-tree/commit/8d2d2dbad257fa6ac6b68d93a9c6c64c10531fbb))
* **registry:** bound heavy-op concurrency and enable autostop on prod ([95e2bb8](https://github.com/openktree/open-knowledge-tree/commit/95e2bb8683c19cfa474b16b620002f6a99317b69))


### Bug Fixes

* **registry:** scale prod VM to shared-cpu-2x / 4GB ([d8f8dea](https://github.com/openktree/open-knowledge-tree/commit/d8f8dea2fc91daeb57aa46be78df9e676516b752))
* **registry:** surface decode errors and drop 30s ReadTimeout ([a412c74](https://github.com/openktree/open-knowledge-tree/commit/a412c74c404cba61593b8086232d342996b6723a))

## [1.1.0](https://github.com/openktree/open-knowledge-tree/compare/registry-v1.0.0...registry-v1.1.0) (2026-07-18)


### Features

* **registry:** add filesystem storage backend for VM-only Fly dev deploy ([53cbdd0](https://github.com/openktree/open-knowledge-tree/commit/53cbdd034c75f1947b4cd908f06bfa7894a4c949))


### Bug Fixes

* **registry:** replace minio-go with aws-sdk-go-v2 for R2 compatibility ([b903c4f](https://github.com/openktree/open-knowledge-tree/commit/b903c4f7daea823421d506eb4af9f01f817533c9))
* **registry:** restore [build] image line in fly.toml for pipeline deploys ([990d4bb](https://github.com/openktree/open-knowledge-tree/commit/990d4bbdfbaff10263b71358cada1634ea643bd6))

## 1.0.0 (2026-07-17)


### Features

* **registry:** add filesystem storage backend for VM-only Fly dev deploy ([53cbdd0](https://github.com/openktree/open-knowledge-tree/commit/53cbdd034c75f1947b4cd908f06bfa7894a4c949))

## Changelog (registry)

Releases are tagged `registry-v<semver>` and published as `ghcr.io/openktree/registry:<version>`.
