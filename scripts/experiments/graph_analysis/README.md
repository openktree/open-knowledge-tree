# Graph-Weakness Experiments

Read-only experiments that turn the OKT paper's "designed but not executed" /
"conceded but never measured" graph claims into evidence. **No OKT code, schema,
migration, or task changes.** See `PLAN.md` for the full plan.

## Prerequisites

- The OKT dev stack running (docker compose up): Postgres on :5432, API on :8080.
- Python 3.11+ with: `asyncpg`, `numpy`, `scipy`, `networkx`, `powerlaw`.
  Install: `pip install asyncpg numpy scipy networkx powerlaw`
- The `multihoprag` repository must be ingested (it already is on the dev DB).

## Running

Each experiment is standalone:

```bash
cd scripts/experiments/graph_analysis

# Experiment 1 — BiCM null-model edge validation
python3 exp1_bicm.py

# Override the DB DSN or repo slug via env if needed:
OKT_DB_DSN=postgres://okt:okt_dev@localhost:5432/okt OKT_REPO_SLUG=multihoprag python3 exp1_bicm.py
```

Results are written to `results/<experiment>.json` (gitignored) and a summary
table is printed to stdout.

## Access mode

Direct read-only SQL against the dev Postgres (:5432) via `asyncpg`. The
`multihoprag` repo lives there; the test DB on :5433 is empty unless the e2e
harness has run. The AGENTS.md prohibition is about the e2e harness that DROPs
schemas on the test DB — these experiments are pure read-only SELECTs against
the dev DB, the same data the existing `multihop_rag` experiments read via the
API on :8080. Direct SQL is just faster than paginated REST for full-graph
dumps.

## Status

| Experiment | Status | Output |
|---|---|---|
| 1 — BiCM null-model edge validation | done | `results/bicm_validation.json` |
| 2 — 7 graph-property measurements | done | `results/graph_properties.json` |
| 3 — 5 failure-mode audits | done (DeepSeek; Gemma comparison pending) | `results/failure_audits.json` |
| 4 — KGQA head-to-head | planned | — |
| REPORT.md | in progress (Exp 1+2+3 written) | — |