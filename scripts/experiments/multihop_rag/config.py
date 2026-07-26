"""Env-driven configuration for the MultiHop-RAG benchmark harness.

All settings can be overridden via environment variables. Defaults are
sensible for a local OKT deployment on http://localhost:8080 with the
dedicated `multihoprag` repository.

A `.env` file next to this script (if present) is auto-loaded on import
so you can keep `token=okt_...`, `user=...`, `OKT_AI_PROVIDER=...`,
`OKT_MODEL=...` there instead of exporting them in every shell. The
`.env` is gitignored.
"""
from __future__ import annotations

import os


def _load_dotenv() -> None:
    """Load a .env file next to this script into os.environ.

    Minimal parser: KEY=VALUE lines, ignores blanks and # comments.
    Does NOT override vars already set in the environment (so explicit
    exports win over the file). Aliases a couple of legacy lowercase
    keys (`token`, `user`) to the canonical OKT_* names.
    """
    path = os.path.join(os.path.dirname(__file__), ".env")
    if not os.path.exists(path):
        return
    aliases = {"token": "OKT_TOKEN", "user": "OKT_USER"}
    with open(path, "r", encoding="utf-8") as fh:
        for line in fh:
            s = line.strip()
            if not s or s.startswith("#") or "=" not in s:
                continue
            key, _, val = s.partition("=")
            key = key.strip()
            val = val.strip().strip('"').strip("'")
            if not key:
                continue
            canonical = aliases.get(key, key)
            if canonical not in os.environ:
                os.environ[canonical] = val


_load_dotenv()


def _env(name: str, default: str) -> str:
    return os.environ.get(name, default)


def _env_int(name: str, default: int) -> int:
    raw = os.environ.get(name)
    if raw is None or raw.strip() == "":
        return default
    try:
        return int(raw)
    except ValueError:
        return default


# OKT connection
OKT_BASE = _env("OKT_BASE", "http://localhost:8080").rstrip("/")
OKT_TOKEN = _env("OKT_TOKEN", "")
OKT_REPO_SLUG = _env("OKT_REPO_SLUG", "multihoprag")

# OKT AI provider (for phrase extraction + answer synthesis). Used only when
# LLM_BACKEND=okt. The provider id matches the chat endpoint:
# POST /api/v1/ai/{provider}/chat
OKT_AI_PROVIDER = _env("OKT_AI_PROVIDER", "ollama")
OKT_MODEL = _env("OKT_MODEL", "gpt-4o-mini")

# Direct OpenRouter backend (used when LLM_BACKEND=openrouter). Keeps the
# LLM calls in the experiment logic instead of proxying through OKT, so the
# OKT_TOKEN only needs data-retrieval scopes (concept:read, fact:read).
# OPENROUTER_API_KEY is read from the env; OPENROUTER_BASE defaults to the
# official OpenRouter chat completions endpoint.
LLM_BACKEND = _env("LLM_BACKEND", "openrouter")
OPENROUTER_API_KEY = _env("OPENROUTER_API_KEY", "")
OPENROUTER_BASE = _env("OPENROUTER_BASE", "https://openrouter.ai/api/v1").rstrip("/")
OPENROUTER_MODEL = _env("OPENROUTER_MODEL", "openai/gpt-4o-mini")
OPENROUTER_REFERER = _env("OPENROUTER_REFERER", "https://github.com/anomalyco/open-knowledge-tree-go")
OPENROUTER_TITLE = _env("OPENROUTER_TITLE", "OKT MultiHop-RAG benchmark")

# Pipeline parameters (overridable via CLI flags in run_benchmark.py).
NUM_CONCEPT_QUERIES = _env_int("NUM_CONCEPT_QUERIES", 5)
TOP_N_CONCEPTS = _env_int("TOP_N_CONCEPTS", 5)
FACTS_PER_CONCEPT = _env_int("FACTS_PER_CONCEPT", 10)
FACTS_PER_QUERY = _env_int("FACTS_PER_QUERY", 10)

# LLM call budget.
LLM_TIMEOUT_S = _env_int("LLM_TIMEOUT_S", 120)
HTTP_TIMEOUT_S = _env_int("HTTP_TIMEOUT_S", 30)
MAX_RETRIES = _env_int("MAX_RETRIES", 2)

# Filesystem layout (relative to the script directory).
DATASET_DIR = os.path.join(os.path.dirname(__file__), "dataset")
CORPUS_DIR = os.path.join(DATASET_DIR, "corpus")
QUERIES_PATH = os.path.join(DATASET_DIR, "queries.jsonl")

ANSWERS_DIR = os.path.join(os.path.dirname(__file__), "answers")
RESULTS_DIR = os.path.join(os.path.dirname(__file__), "results")
PREDICTIONS_PATH = os.path.join(RESULTS_DIR, "predictions.jsonl")
QA_METRICS_PATH = os.path.join(RESULTS_DIR, "qa_metrics.json")
SUMMARY_PATH = os.path.join(RESULTS_DIR, "summary.txt")

# ---------------------------------------------------------------------------
# Baseline (Dense X + Traditional RAG) configuration
# ---------------------------------------------------------------------------
# The baselines live in scripts/experiments/multihop_rag/baselines/. They use
# the SAME embedding model OKT uses (google/gemini-embedding-2, 3072-dim) so
# the comparison isolates chunking + retrieval strategy, not embedding model.
# The embedding calls go directly to OpenRouter's /v1/embeddings endpoint
# (the same endpoint OKT's OpenRouterProvider.Embed calls), reusing
# OPENROUTER_API_KEY from the env.
#
# The baselines store vectors in STANDALONE Qdrant collections on the already
# running Qdrant instance (default localhost:6334). Two collections:
#   - multihoprag_passages      (Traditional RAG: fixed-size passage chunks)
#   - multihoprag_propositions  (Dense X: proposition units)
# These are separate from OKT's okt_facts collection; no OKT backend changes
# are needed. The baselines retrieve dense-only (cosine, no lexical/RRF).

# Qdrant connection for the baselines. Defaults to the REST port (6333)
# of the same Qdrant instance OKT uses (config.default.yaml qdrant.host).
# OKT talks to Qdrant over gRPC on 6334, but the baseline Python client
# uses the REST API (search/upsert are HTTP calls), so we point at 6333.
# Override via env if your Qdrant is elsewhere.
BASELINE_QDRANT_HOST = _env("BASELINE_QDRANT_HOST", "localhost")
BASELINE_QDRANT_PORT = _env_int("BASELINE_QDRANT_PORT", 6333)
BASELINE_QDRANT_API_KEY = _env("BASELINE_QDRANT_API_KEY", "")

# Collection names. Prefixed with multihoprag_ so they're easy to find and
# drop. Must NOT collide with OKT's okt_facts / okt_concepts collections.
BASELINE_PASSAGE_COLLECTION = _env(
    "BASELINE_PASSAGE_COLLECTION", "multihoprag_passages"
)
BASELINE_PROPOSITION_COLLECTION = _env(
    "BASELINE_PROPOSITION_COLLECTION", "multihoprag_propositions"
)

# Embedding model + dimensions. MUST match OKT's config (config.default.yaml
# ai.embedding) so the baseline's dense retrieval is comparable to OKT's
# hybrid semantic channel. The baselines call OpenRouter /v1/embeddings
# directly (no OKT proxy), reusing OPENROUTER_API_KEY + OPENROUTER_BASE.
BASELINE_EMBEDDING_MODEL = _env(
    "BASELINE_EMBEDDING_MODEL", "google/gemini-embedding-2"
)
BASELINE_EMBEDDING_DIMENSIONS = _env_int("BASELINE_EMBEDDING_DIMENSIONS", 3072)

# Embedding batch size for the index-build step. OpenRouter returns an empty
# data array when a batch exceeds the underlying model's input-count or
# total-token limit; we auto-halve on that signal (mirroring OKT's
# embedBatchRecursive). 32 matches OKT's defaultOpenRouterEmbedBatchSize.
BASELINE_EMBED_BATCH_SIZE = _env_int("BASELINE_EMBED_BATCH_SIZE", 32)

# Traditional-RAG chunking. Fixed-size passage chunks with overlap. 512 tokens
# is the canonical RAG default; 50-token overlap preserves cross-chunk
# context. Tokenized by whitespace (news articles are English; a simple
# whitespace split is sufficient and avoids a tokenizer dependency).
BASELINE_PASSAGE_TOKENS = _env_int("BASELINE_PASSAGE_TOKENS", 512)
BASELINE_PASSAGE_OVERLAP = _env_int("BASELINE_PASSAGE_OVERLAP", 50)

# Dense X proposition extraction. The Dense X paper releases a propositionizer
# model (chentong00/propositionizer-windows on HuggingFace). Two modes:
#   - "model": run the released propositionizer (requires transformers +
#     torch; slow on CPU but faithful to the paper). Downloaded once.
#   - "llm":   fall back to an LLM-based proposition extractor using the
#     configured synthesis LLM (gemma). Faster, no torch dep, slightly less
#     faithful but still produces atomic self-contained propositions.
# Default "llm" for portability; set BASELINE_PROPOSITIONIZER=model for the
# faithful paper reproduction.
BASELINE_PROPOSITIONIZER = _env("BASELINE_PROPOSITIONIZER", "llm")

# Retrieval: dense-only cosine. min_score is the cosine similarity floor
# (Qdrant returns similarity as score; 0.0 = orthogonal, 1.0 = identical).
# 0.0 keeps all top-k matches; raising it prunes weak matches. We keep 0.0
# so the baseline always returns k chunks (matching OKT's behavior of
# always returning k facts when available).
BASELINE_MIN_SCORE = float(_env("BASELINE_MIN_SCORE", "0.0"))

# Concurrency for the index-build embedding step (independent of the
# per-question run concurrency, which is set via run_baseline.py --concurrency).
BASELINE_INDEX_CONCURRENCY = _env_int("BASELINE_INDEX_CONCURRENCY", 8)

# ---------------------------------------------------------------------------
# Late-chunking baseline configuration
# ---------------------------------------------------------------------------
# Late chunking (arXiv:2409.04701) embeds a full document through a
# long-context encoder to get per-token embeddings, then chunks AFTER the
# transformer forward pass and BEFORE mean pooling. This structurally
# requires a long-context embedding model that exposes pre-pooling token
# vectors — NOT the gemini-embedding-2 model used by the other baselines /
# OKT (which returns pooled sentence vectors via OpenRouter /v1/embeddings).
# This is an inherent methodological asymmetry, NOT a bug: the experiment
# isolates (chunking strategy + its native embedding), not chunking alone.
# Flagged explicitly in the writeup per the experiment plan's success
# criterion #3.
#
# Default model: jina-embeddings-v3 (the model the late-chunking paper
# itself uses). Self-hosted via transformers — NO API key required. The
# model is public (CC-BY-NC, fine for research), downloaded once by
# HuggingFace (~2GB), and kept resident on the GPU. You already have
# torch+CUDA+transformers installed.
BASELINE_LATE_CHUNK_COLLECTION = _env(
    "BASELINE_LATE_CHUNK_COLLECTION", "multihoprag_late_chunks"
)
# HuggingFace model id for the long-context embedding model. jinaai/
# jina-embeddings-v3: 570M params, 8192-token context, 1024-dim, exposes
# per-token hidden states (the precondition for late chunking).
BASELINE_LATE_CHUNK_MODEL = _env(
    "BASELINE_LATE_CHUNK_MODEL", "jinaai/jina-embeddings-v3"
)
BASELINE_LATE_CHUNK_DIMENSIONS = _env_int("BASELINE_LATE_CHUNK_DIMENSIONS", 1024)
BASELINE_LATE_CHUNK_MAX_TOKENS = _env_int("BASELINE_LATE_CHUNK_MAX_TOKENS", 8192)
# Chunk window size (whitespace tokens) applied AFTER the transformer
# forward pass, BEFORE mean pooling. Tuned to match the average OKT atomic
# fact length (measured at ~23 whitespace tokens). See Phase 3 of the
# experiment plan. Keep within 1x-2x of the fact average for a clean
# granularity match; flag any divergence in the writeup.
BASELINE_LATE_CHUNK_WINDOW = _env_int("BASELINE_LATE_CHUNK_WINDOW", 24)
# Per-document embedding batch size for the index build (documents per
# forward pass). Each doc is one forward pass; this controls how many
# docs to queue in parallel — but the model is resident on the GPU and
# forward passes are serial, so this only affects the worker count for
# the ThreadPoolExecutor in index_build.py. Default 4 keeps GPU memory
# bounded (each forward pass holds one doc's per-token activations).
BASELINE_LATE_CHUNK_BATCH_SIZE = _env_int("BASELINE_LATE_CHUNK_BATCH_SIZE", 4)