#!/usr/bin/env python3
"""
DBpedia L3 label merging experiment.

Embeds all 789 DBpedia L3 ontology class labels with the same embedding
model the OKT system uses (google/gemini-embedding-2 via OpenRouter),
falls back to a local HF model (BAAI/bge-large-en-v1.5) when OpenRouter
is unavailable, then agglomeratively clusters the labels by cosine
distance and merges them until a set of target cluster counts is
reached. For each target it records which labels were combined and
which representative label names the cluster, and reports the
estimated per-fact prompt-token savings.

Outputs (written next to the script, or to OUT_DIR if set):
  label_merging_result.json   full merge map at every target
  merge_dendrogram.png        distance-vs-count knee plot
  label_projection.png        2D cluster scatter at target 150
  stdout                      summary table + biggest clusters

Read-only with respect to the repo: only reads dbpedia_l3.json.

Usage:
  # from repo root, with OpenRouter configured in .env
  set -a; . .env; python3 scripts/experiments/merge_labels.py

  # or with an explicit labels path / output dir
  python3 scripts/experiments/merge_labels.py \
      --labels backend/internal/providers/ontology/dbpedia_l3.json \
      --out-dir /tmp/opencode
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.request
import urllib.error
from pathlib import Path

import numpy as np
from scipy.cluster.hierarchy import linkage, fcluster
from scipy.spatial.distance import squareform
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
from sklearn.decomposition import PCA
from sklearn.manifold import TSNE

DEFAULT_LABELS_PATH = (
    Path(__file__).resolve().parents[2]
    / "backend/internal/providers/ontology/dbpedia_l3.json"
)
DEFAULT_OUT_DIR = Path(__file__).resolve().parent
TARGETS = [789, 400, 200, 150, 100, 50]
PROJECTION_TARGET = 150

OPENROUTER_URL = "https://openrouter.ai/api/v1/embeddings"
OPENROUTER_MODEL = "google/gemini-embedding-2"
HF_FALLBACK_MODEL = "BAAI/bge-large-en-v1.5"


def load_labels(path: Path) -> list[str]:
    with open(path) as f:
        labels = json.load(f)
    if not isinstance(labels, list) or not labels:
        raise SystemExit(f"unexpected label file shape: {type(labels)}")
    return labels


def embed_openrouter(labels: list[str]) -> tuple[np.ndarray, str]:
    api_key = os.environ.get("OPENROUTER_API_KEY", "")
    if not api_key:
        raise RuntimeError("OPENROUTER_API_KEY not set")
    # OpenRouter caps the embeddings input array somewhere below 128
    # entries (gemini-embedding-2 returned empty data above ~96), so
    # chunk the request well below the limit and concatenate.
    chunk = 64
    all_vecs: list[list[float]] = []
    for i in range(0, len(labels), chunk):
        batch = labels[i : i + chunk]
        body = json.dumps({"model": OPENROUTER_MODEL, "input": batch}).encode()
        req = urllib.request.Request(
            OPENROUTER_URL,
            data=body,
            headers={
                "Authorization": f"Bearer {api_key}",
                "Content-Type": "application/json",
            },
            method="POST",
        )
        with urllib.request.urlopen(req, timeout=180) as resp:
            payload = json.loads(resp.read())
        if "data" not in payload or not payload["data"]:
            raise RuntimeError(
                f"OpenRouter returned no embeddings for batch [{i}:{i+len(batch)}]; "
                f"payload keys={list(payload.keys())}"
            )
        batch_data = sorted(payload["data"], key=lambda d: d.get("index", 0))
        for d in batch_data:
            all_vecs.append(d["embedding"])
    if len(all_vecs) != len(labels):
        raise RuntimeError(
            f"OpenRouter returned {len(all_vecs)} vectors for {len(labels)} labels"
        )
    vecs = np.array(all_vecs, dtype=np.float32)
    return vecs, OPENROUTER_MODEL


def embed_hf_fallback(labels: list[str]) -> tuple[np.ndarray, str]:
    import torch
    from transformers import AutoTokenizer, AutoModel

    tok = AutoTokenizer.from_pretrained(HF_FALLBACK_MODEL)
    model = AutoModel.from_pretrained(HF_FALLBACK_MODEL)
    model.eval()
    vecs = []
    batch = 32
    with torch.no_grad():
        for i in range(0, len(labels), batch):
            chunk = labels[i : i + batch]
            enc = tok(chunk, padding=True, truncation=True, max_length=64, return_tensors="pt")
            out = model(**enc)
            mask = enc["attention_mask"].unsqueeze(-1).float()
            summed = (out.last_hidden_state * mask).sum(1)
            counts = mask.sum(1).clamp(min=1e-9)
            emb = summed / counts
            vecs.append(emb.cpu().numpy())
    arr = np.concatenate(vecs, axis=0).astype(np.float32)
    return arr, HF_FALLBACK_MODEL


def embed(labels: list[str]) -> tuple[np.ndarray, str]:
    try:
        return embed_openrouter(labels)
    except Exception as e:
        print(
            f"[fallback] OpenRouter embedding failed ({e}); "
            f"using local HF model {HF_FALLBACK_MODEL}",
            file=sys.stderr,
        )
        return embed_hf_fallback(labels)


def cosine_distance_matrix(vecs: np.ndarray) -> np.ndarray:
    norms = np.linalg.norm(vecs, axis=1, keepdims=True)
    norms = np.where(norms == 0, 1e-12, norms)
    normed = vecs / norms
    sim = normed @ normed.T
    sim = np.clip(sim, -1.0, 1.0)
    return 1.0 - sim


def representative_label(members_idx: list[int], vecs: np.ndarray, labels: list[str]) -> str:
    if len(members_idx) == 1:
        return labels[members_idx[0]]
    sub = vecs[members_idx]
    centroid = sub.mean(axis=0)
    cnorm = np.linalg.norm(centroid)
    if cnorm == 0:
        cnorm = 1e-12
    sims = (sub @ centroid) / (np.linalg.norm(sub, axis=1) * cnorm)
    best = members_idx[int(np.argmax(sims))]
    return labels[best]


def build_clusters(Z: np.ndarray, vecs: np.ndarray, labels: list[str], target: int) -> list[dict]:
    cluster_ids = fcluster(Z, t=target, criterion="maxclust")
    groups: dict[int, list[int]] = {}
    for li, ci in enumerate(cluster_ids):
        groups.setdefault(int(ci), []).append(int(li))
    clusters = []
    for members_idx in groups.values():
        rep = representative_label(members_idx, vecs, labels)
        clusters.append({
            "representative": rep,
            "members": [labels[i] for i in members_idx],
            "size": len(members_idx),
        })
    clusters.sort(key=lambda c: -c["size"])
    return clusters


def est_prompt_tokens(n_labels: int, avg_label_len: float) -> int:
    # '- <label>\n' per line, plus the static prompt template (~2470 chars)
    list_chars = int(n_labels * (avg_label_len * 1.3 + 4))
    total = 2470 + list_chars + 40  # +40 for fact stub
    return total // 4


def plot_dendrogram_knee(Z: np.ndarray, out: Path):
    n = Z.shape[0] + 1
    counts = list(range(n, 1, -1))
    distances = Z[:, 2]
    fig, ax = plt.subplots(figsize=(10, 6))
    ax.plot(counts, distances, marker=".")
    ax.set_xlabel("Number of clusters remaining")
    ax.set_ylabel("Merge distance (cosine)")
    ax.set_title("Agglomerative merge trace - knee = where distance jumps")
    ax.invert_xaxis()
    for t in TARGETS:
        if t < n:
            ax.axvline(t, color="r", linestyle="--", alpha=0.4)
            ax.annotate(f"t={t}", xy=(t, distances.max() * 0.95), color="r", fontsize=8, ha="right")
    fig.tight_layout()
    fig.savefig(out, dpi=120)
    plt.close(fig)


def plot_projection(vecs: np.ndarray, Z: np.ndarray, labels: list[str], target: int, out: Path, model_name: str):
    cluster_ids = fcluster(Z, t=target, criterion="maxclust")
    red = vecs
    if vecs.shape[1] > 30:
        red = PCA(n_components=30, random_state=0).fit_transform(vecs)
    perplexity = min(30, len(labels) - 1)
    coords = TSNE(n_components=2, init="pca", perplexity=perplexity, random_state=0).fit_transform(red)

    groups: dict[int, list[int]] = {}
    for li, ci in enumerate(cluster_ids):
        groups.setdefault(int(ci), []).append(li)
    rep_of = {}
    for ci, members in groups.items():
        rep_of[ci] = representative_label(members, vecs, labels)

    fig, ax = plt.subplots(figsize=(14, 10))
    palette = plt.cm.tab20(np.linspace(0, 1, min(len(groups), 20)))
    colors = np.tile(palette, (len(groups) // 20 + 1, 1))[: len(groups)]
    for (ci, members), col in zip(sorted(groups.items(), key=lambda kv: -len(kv[1])), colors):
        xs = coords[members, 0]
        ys = coords[members, 1]
        ax.scatter(xs, ys, s=18, color=[col], alpha=0.7, label=None)
        if len(members) >= 3:
            cx, cy = coords[members].mean(axis=0)
            ax.annotate(rep_of[ci], (cx, cy), fontsize=6, alpha=0.85, ha="center")
    ax.set_title(
        f"DBpedia L3 label embedding projection (target={target}, model={model_name})\n"
        f"labels annotated = cluster representative"
    )
    ax.set_xlabel("t-SNE dim 1")
    ax.set_ylabel("t-SNE dim 2")
    fig.tight_layout()
    fig.savefig(out, dpi=120)
    plt.close(fig)


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--labels", type=Path, default=DEFAULT_LABELS_PATH,
                    help=f"path to dbpedia_l3.json (default: {DEFAULT_LABELS_PATH})")
    ap.add_argument("--out-dir", type=Path, default=DEFAULT_OUT_DIR,
                    help=f"output directory (default: {DEFAULT_OUT_DIR})")
    args = ap.parse_args()

    out_dir: Path = args.out_dir
    out_dir.mkdir(parents=True, exist_ok=True)

    labels = load_labels(args.labels)
    print(f"loaded {len(labels)} labels from {args.labels}")
    avg_label_len = sum(len(x) for x in labels) / len(labels)

    vecs, model_name = embed(labels)
    print(f"embedded {len(labels)} labels with {model_name} (dim={vecs.shape[1]})")

    D = cosine_distance_matrix(vecs)
    np.fill_diagonal(D, 0.0)
    condensed = squareform(D, checks=False)
    Z = linkage(condensed, method="average")

    plot_dendrogram_knee(Z, out_dir / "merge_dendrogram.png")
    plot_projection(vecs, Z, labels, PROJECTION_TARGET, out_dir / "label_projection.png", model_name)

    result = {
        "embedding_model": model_name,
        "embedding_dim": int(vecs.shape[1]),
        "label_count": len(labels),
        "targets": {},
    }

    print("\n" + "=" * 92)
    print(f"{'target':>7} {'clusters':>8} {'compr':>6} {'est_tok/fact':>12} {'saved':>7} {'biggest cluster (rep -> members)':<50}")
    print("-" * 92)
    for t in TARGETS:
        clusters = build_clusters(Z, vecs, labels, t)
        est_tok = est_prompt_tokens(len(clusters), avg_label_len)
        baseline_tok = est_prompt_tokens(len(labels), avg_label_len)
        saved = baseline_tok - est_tok
        compr = len(labels) / len(clusters) if len(clusters) else 0
        biggest = clusters[0] if clusters else None
        bstr = ""
        if biggest:
            mem = ", ".join(biggest["members"][:6])
            if len(biggest["members"]) > 6:
                mem += f", +{len(biggest['members']) - 6} more"
            bstr = f"{biggest['representative']} -> [{mem}]"
        print(f"{t:>7} {len(clusters):>8} {compr:>6.1f}x {est_tok:>12} {saved:>7} {bstr:<50}")
        result["targets"][str(t)] = {
            "cluster_count": len(clusters),
            "compression": round(compr, 2),
            "est_prompt_tokens_per_fact": est_tok,
            "baseline_tokens_per_fact": baseline_tok,
            "tokens_saved_per_fact": saved,
            "clusters": clusters,
        }

    with open(out_dir / "label_merging_result.json", "w") as f:
        json.dump(result, f, indent=2)
    print("=" * 92)
    print(f"\nwrote {out_dir/'label_merging_result.json'}")
    print(f"wrote {out_dir/'merge_dendrogram.png'}")
    print(f"wrote {out_dir/'label_projection.png'}")

    # Print the top-10 biggest merged clusters at the projection target.
    t_key = str(PROJECTION_TARGET)
    t_block = result["targets"].get(t_key) or result["targets"][t_key]
    print(f"\nTop 10 biggest merged clusters at target {PROJECTION_TARGET}:")
    print("-" * 92)
    for c in t_block["clusters"][:10]:
        mem = ", ".join(c["members"])
        print(f"  [{c['size']:2d}] {c['representative']} <- {mem}")


if __name__ == "__main__":
    main()