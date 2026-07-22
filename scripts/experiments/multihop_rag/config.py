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