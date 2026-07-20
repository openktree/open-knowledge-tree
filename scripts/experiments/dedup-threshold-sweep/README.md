# Dedup threshold sweep

Standalone experiment that connects directly to the local Qdrant (6333 REST)
and Postgres (5432) to replay the `deduplicate_facts` worker's nearest-neighbor
search for every embedded fact of a single investigation, then sweeps candidate
thresholds to estimate how many facts would be marked `to_delete` at each level
and surfaces example pairs so an operator can pick the right `dedup.threshold`
for the current embedding model.

Read-only:
- Qdrant: only `query_points` (recommend-by-id) — no upserts/deletes.
- Postgres: only `SELECT` — no writes, no advisory locks.

The script does NOT boot the API server, River workers, or run migrations.
Only the `postgres` (port 5432) and `qdrant` (port 6333) Docker containers
need to be up.

## Requirements

Already installed for `python3.10` on this machine:

- `psycopg2-binary`
- `qdrant-client`

(`python3` is 3.11 here and lacks psycopg2 — use `python3.10` explicitly.)

## Usage

```bash
python3.10 dedup_threshold_sweep.py \
    --investigation 12c7ba67-9b61-424a-85c5-d5206b32af52 \
    --out reports/dedup_sweep_shadow_fleet.html \
    --concurrency 8 \
    --top-k 20 \
    --examples-per-band 8
```

Flags:

| Flag | Default | Purpose |
|------|---------|---------|
| `--investigation` | required | investigation UUID |
| `--pg-host/--pg-port/--pg-user/--pg-password/--pg-db` | localhost:5432 okt/okt_dev/okt | Postgres connection |
| `--qdrant-host/--qdrant-port/--qdrant-collection` | localhost:6333/okt_facts | Qdrant connection (6333 = REST) |
| `--top-k` | 20 | neighbors fetched per fact |
| `--concurrency` | 8 | parallel Qdrant queries |
| `--thresholds` | 0.80,0.82,...,0.98 | candidate thresholds to evaluate |
| `--examples-per-band` | 6 | example pairs to show per threshold band |
| `--out` | dedup_sweep.html | HTML report output path |
| `--max-facts` | 0 (all) | cap facts scanned |
| `--seed` | 42 | example sampling seed |

Outputs:
- `<out>.html` — human-readable report (stat cards, histogram, per-threshold table with both any-neighbor and cross-source-only views, example pairs per band).
- `<out>.json` — raw sweep data (neighbor map, stats, histogram, quantiles) for re-use.

## What the report shows

For each threshold T the report computes two views:

1. **Any-neighbor** — facts whose nearest neighbor (anywhere in the repo) is ≥ T.
   This is the *upper bound* on what a threshold-only dedup could collapse if it
   ignored source attribution.
2. **Cross-source only** — facts whose nearest neighbor ≥ T comes from a
   *disjoint* set of sources. This matches the current `deduplicate_facts`
   worker, which only ever merges facts across different sources — two
   near-duplicate facts extracted from the *same* source are never merged.

Comparing the two views surfaces the same-source dedup gap: for the Shadow
Fleet investigation at T=0.94 (the current production threshold), 1,010 facts
have a neighbor ≥ 0.94, but only 64 of those come from a different source —
the other 946 are near-duplicates within the same source that the current
worker will never merge.

The example-pairs section shows 8 random (fact, nearest-neighbor, score)
triples per band so an operator can eyeball whether the merges look correct
at that level. Green = near-identical text, yellow = paraphrase / overlapping
subject, red = likely distinct facts (false-positive risk if threshold is
set there).

## Existing reports

- `reports/dedup_sweep_shadow_fleet.html` — investigation *Shadow Fleet —
  Fleet-size & volume estimate conflicts* (139 sources, 4,427 embedded
  text-stable facts), `gemini-embedding-2` model, 12.5s sweep runtime.