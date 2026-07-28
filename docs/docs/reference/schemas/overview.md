---
id: overview
sidebar_position: 0
title: Interchange Schemas
---

# Interchange Schemas

OKT's value compounds when the artifacts it produces — sources, facts, concepts, embeddings, summaries, syntheses — can move between instances without re-running the expensive chat-model pipeline. **Interchange schemas** are the serialized shapes that make that possible. They are the contract every OKT instance (and, by design, any compatible third-party system) can emit and consume.

There are two interchange formats, layered by scope:

| Format | Scope | Lives in | Typical size |
|--------|-------|----------|--------------|
| **Decomposition Package** | one source | Knowledge Registry (S3 + metadata DB) | small JSON (KB–low MB) |
| **Graph Bundle** | one whole repository | Knowledge Registry (S3, gzipped) | large (MB–GB) |

Both formats share the same design principles:

- **Self-contained.** A consumer with only the package/bundle file can reconstruct the derived layer without any other network call (except optional re-embedding when the embedding model differs).
- **Stable, versioned.** Each carries a `schema_version`. The importer refuses bundles newer than it understands and treats older ones with a defined upgrade path.
- **Dedup-friendly.** Every fact carries a `content_hash` (SHA-256 of its normalized text) so two instances that decompose the same source with the same model converge on the same fact identity. The registry's `fact_hashes` table indexes these hashes for cross-source linking.
- **Promptset-tagged.** Each artifact carries a `promptset_hash` identifying the philosophy that produced it — the decomposition package carries one hash at the package level (every fact in a decomposition shares the same promptset); the graph bundle carries it per fact/concept/link plus a roll-up list in `metadata.promptset_hashes`. A consumer that only accepts certain philosophies can filter by hash before importing, so an instance never silently mixes incompatible decomposition styles. The registry can optionally **enforce** the tag on push via `promptset.enable_validation` (default off for backward compatibility).
- **Model-tagged.** Embeddings carry their `model` and `dimensions`; facts carry the chat `model_id` that decomposed them. A consumer always knows whether its vectors are compatible or need re-embedding.
- **Transport-agnostic.** The schemas are pure JSON documents. They flow over HTTP today, but nothing in the format assumes HTTP — a file dump, an air-gap sneakernet, or a future gRPC stream all carry the same bytes.

## Why standardize?

The decomposition package in particular is intentionally minimal and model-agnostic. If a third-party research tool emits the same shape — a list of self-contained facts with content hashes, the concepts it extracted, and an optional embeddings block — any OKT instance can import it directly and skip the fetch + decompose cost. The format is the integration point; the OKT backend and the Knowledge Registry are just one producer and one broker for it.

The graph bundle goes one step further: it packages an entire repository's derived layer (sources, facts, concepts, summaries, syntheses, investigations, reports, embeddings, and the source bodies/images) so a fresh OKT instance can stand up a complete knowledge graph in a single import job with zero LLM cost. This is the format that turns a curated, researched repository into a portable, shareable artifact.

## Where the schemas live in code

| Format | Source of truth |
|--------|-----------------|
| Decomposition Package | `registry/internal/model/types.go` — `DecompositionPackage` and its sub-structs |
| Graph Bundle | `backend/internal/providers/graph/bundle.go` — `GraphBundle` and its sub-structs |

The registry is the canonical producer/consumer of the decomposition package; the OKT backend's `providers/graph` package is the canonical producer/consumer of the graph bundle. Both schemas are JSON-serialized via the standard `encoding/json` tags shown on each struct.

## In this section

- [Decomposition Package](/docs/reference/schemas/decomposition-package) — the per-source interchange format pushed to and pulled from the Knowledge Registry.
- [Graph Bundle](/docs/reference/schemas/graph-bundle) — the per-repository interchange format for whole-graph export and import.