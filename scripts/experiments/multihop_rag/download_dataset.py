"""Download the MultiHop-RAG dataset from HuggingFace and stage it locally.

Outputs:
  dataset/corpus/<slug>.md    — one markdown file per corpus article,
                                title as filename, YAML metadata header,
                                article body below.
  dataset/queries.jsonl        — one JSON line per query:
                                {id, query, question_type, gold_answer}

The corpus files are meant to be uploaded by the user to the OKT
`multihoprag` repository (UI upload). This script does NOT touch OKT.

Idempotent: skips files/queries that already exist. Safe to re-run.

Dataset license: ODC-BY 1.0 (https://opendatacommons.org/licenses/by/1-0/)
Paper: Tang & Yang, "MultiHop-RAG: Benchmarking Retrieval-Augmented
Generation for Multi-Hop Queries", COLM 2024. https://arxiv.org/abs/2401.15391
"""
from __future__ import annotations

import json
import os
import re
import sys

import config


def _slugify(title: str, max_len: int = 80) -> str:
    """Turn a title into a filesystem-safe lowercase slug."""
    s = title.strip().lower()
    # Replace non-alphanumeric runs with hyphens.
    s = re.sub(r"[^a-z0-9]+", "-", s)
    s = s.strip("-")
    if not s:
        s = "untitled"
    if len(s) > max_len:
        s = s[:max_len].rstrip("-")
    return s


def _safe_filename(slug: str, seen: set[str]) -> str:
    """De-dup slug with a short hash suffix when collisions occur."""
    if slug not in seen:
        seen.add(slug)
        return f"{slug}.md"
    # Append a short numeric suffix.
    i = 2
    while f"{slug}-{i}" in seen:
        i += 1
    name = f"{slug}-{i}"
    seen.add(name)
    return f"{name}.md"


def _first_text_field(row: dict, candidates: tuple[str, ...]) -> str:
    """Return the first non-empty string field among candidate names."""
    for key in candidates:
        v = row.get(key)
        if isinstance(v, str) and v.strip():
            return v
    return ""


def download_corpus() -> int:
    """Write one .md per corpus article. Returns count written."""
    from datasets import load_dataset

    os.makedirs(config.CORPUS_DIR, exist_ok=True)
    print("Loading MultiHopRAG corpus from HuggingFace...")
    ds = load_dataset("yixuantt/MultiHopRAG", "corpus", split="train")
    print(f"  corpus rows: {len(ds)}")

    seen: set[str] = set()
    written = 0
    skipped = 0
    for row in ds:
        title = (row.get("title") or "").strip() or "untitled"
        slug = _slugify(title)
        fname = _safe_filename(slug, seen)
        path = os.path.join(config.CORPUS_DIR, fname)
        if os.path.exists(path):
            skipped += 1
            continue

        body = _first_text_field(
            row, ("content", "body", "text", "article", "markdown")
        )
        meta_lines = [
            "---",
            f'title: {json.dumps(title)}',
            f'source: {json.dumps(row.get("source") or "")}',
            f'author: {json.dumps(row.get("author") or "")}',
            f'category: {json.dumps(row.get("category") or "")}',
            f'published_at: {json.dumps(str(row.get("published_at") or ""))}',
            "---",
            "",
            body.strip(),
            "",
        ]
        with open(path, "w", encoding="utf-8") as fh:
            fh.write("\n".join(meta_lines))
        written += 1
    print(f"  wrote {written} corpus .md files ({skipped} already present)")
    return written


def download_queries() -> int:
    """Write dataset/queries.jsonl. Returns count written."""
    from datasets import load_dataset

    os.makedirs(config.DATASET_DIR, exist_ok=True)
    print("Loading MultiHopRAG queries from HuggingFace...")
    ds = load_dataset("yixuantt/MultiHopRAG", "MultiHopRAG", split="train")
    print(f"  query rows: {len(ds)}")

    # Load existing ids so we can resume.
    existing: set[str] = set()
    if os.path.exists(config.QUERIES_PATH):
        with open(config.QUERIES_PATH, "r", encoding="utf-8") as fh:
            for line in fh:
                try:
                    existing.add(json.loads(line)["id"])
                except Exception:  # noqa: BLE001
                    pass

    written = 0
    with open(config.QUERIES_PATH, "a", encoding="utf-8") as fh:
        for i, row in enumerate(ds):
            qid = f"{i:04d}"
            if qid in existing:
                continue
            obj = {
                "id": qid,
                "query": row.get("query") or "",
                "question_type": row.get("question_type") or "",
                "gold_answer": row.get("answer") or "",
            }
            fh.write(json.dumps(obj, ensure_ascii=False) + "\n")
            written += 1
    print(f"  wrote {written} queries ({len(existing)} already present)")
    print(f"  queries file: {config.QUERIES_PATH}")
    return written


def main() -> int:
    print(
        "MultiHop-RAG dataset downloader\n"
        "  dataset: https://huggingface.co/datasets/yixuantt/MultiHopRAG\n"
        "  license: ODC-BY 1.0\n"
        "  paper:  https://arxiv.org/abs/2401.15391\n"
    )
    try:
        download_corpus()
    except Exception as e:  # noqa: BLE001
        print(f"  corpus download failed: {e}", file=sys.stderr)
    try:
        download_queries()
    except Exception as e:  # noqa: BLE001
        print(f"  queries download failed: {e}", file=sys.stderr)
    print("\nNext step: upload dataset/corpus/*.md to the OKT `multihoprag` repo.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())