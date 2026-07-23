"""Rerun only Gemma's LLM audits and patch into existing failure_audits.json.

DeepSeek already ran (results in failure_audits.json), but Gemma's results
were cached copies of DeepSeek (same cache key bug, now fixed). This script
runs ONLY Gemma's Failure 2 + Failure 5 and updates the JSON.
"""
from __future__ import annotations

import asyncio
import json
import os
import time
from pathlib import Path

# Load env from multihop_rag/.env
_env = Path("/home/charlie/Documents/workspace/love/open-knowledge-tree-go/scripts/experiments/multihop_rag/.env")
for line in _env.read_text().splitlines():
    s = line.strip()
    if not s or s.startswith("#") or "=" not in s:
        continue
    key, _, val = s.partition("=")
    os.environ[key.strip()] = val.strip().strip('"').strip("'")

import asyncpg
from exp3_failure_audits import audit_over_merging, audit_context_mislabeling
from okt_db import DEFAULT_DSN, DEFAULT_REPO_SLUG, repo_id_for_slug

RESULTS = Path(__file__).resolve().parent / "results" / "failure_audits.json"


async def main():
    result = json.loads(RESULTS.read_text())
    conn = await asyncpg.connect(DEFAULT_DSN)
    rid = await repo_id_for_slug(conn, DEFAULT_REPO_SLUG)

    print("Rerunning Failure 2 — Over-merging (gemma)...")
    t0 = time.time()
    f2 = await audit_over_merging(conn, rid, model="google/gemma-4-31b-it")
    print(f"  done in {time.time()-t0:.0f}s: {f2['n_over_merged']} over-merged")

    print("Rerunning Failure 5 — Context mislabeling (gemma)...")
    t0 = time.time()
    f5 = await audit_context_mislabeling(conn, rid, model="google/gemma-4-31b-it")
    print(f"  done in {time.time()-t0:.0f}s: {f5['n_mislabeled']} mislabeled")

    result["failure_2_over_merging_by_model"]["gemma-4-31b-it"] = f2
    result["failure_5_context_mislabeling_by_model"]["gemma-4-31b-it"] = f5

    # Compare
    f2d = result["failure_2_over_merging_by_model"]["deepseek-v4-flash"]["all_audits"]
    f2g = f2["all_audits"]
    diffs2 = sum(1 for a, b in zip(f2d, f2g) if a != b)
    print(f"\nF2 deepseek vs gemma: {diffs2} differing judgments / {len(f2g)}")

    f5d = result["failure_5_context_mislabeling_by_model"]["deepseek-v4-flash"]["all_audits"]
    f5g = f5["all_audits"]
    diffs5 = sum(1 for a, b in zip(f5d, f5g) if a != b)
    print(f"F5 deepseek vs gemma: {diffs5} differing judgments / {len(f5g)}")

    RESULTS.write_text(json.dumps(result, indent=2))
    print(f"\nUpdated {RESULTS}")
    await conn.close()


if __name__ == "__main__":
    asyncio.run(main())