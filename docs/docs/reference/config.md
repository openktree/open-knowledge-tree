---
id: config
sidebar_position: 5
title: Configuration
---

# Configuration Reference

OKT loads configuration from a layered YAML file. The defaults are embedded in the binary and auto-written to `<binary_dir>/configs/config.default.yaml` on first run when no on-disk copy exists.

Override values by creating `configs/config.local.yaml` (gitignored) alongside the binary. The load order is:

1. `configs/config.default.yaml` (embedded fallback)
2. `configs/config.local.yaml` (local overrides)
3. Environment variables (highest priority)

You can also point to a custom config path with `--config <file|dir>`.

## Example config

A minimal `config.local.yaml` for a typical local setup:

```yaml
server:
  port: 8080

databases:
  default:
    host: localhost
    port: 5432
    user: okt
    password: okt_dev
    name: okt
    ssl_mode: disable
  tasks:
    host: localhost
    port: 5432
    user: okt
    password: okt_dev
    name: okt_tasks
    ssl_mode: disable

system:
  database: default

task:
  database: tasks
  job_timeout: 4h
  queues:
    retrieve_source: 20
    source_decomposition: 100
    embed_facts: 50
    deduplicate_facts: 50
    extract_concepts: 100

auth:
  jwt_secret: "change-me-in-production"
  token_ttl: 24h

providers:
  search:
    provider: "serper"
    serper:
      api_key: "your-serper-key"
  decomposition:
    fact_extraction:
      provider: "openrouter"
      model: "google/gemma-4-31b-it"
    concept_extraction:
      enabled: true
      provider: "openrouter"
      model: "google/gemma-4-31b-it"
  embedding:
    provider: "openrouter"
    model: "google/gemini-embedding-2"
    dimensions: 3072
  qdrant:
    host: localhost
    port: 6334
  ai:
    openrouter:
      api_key: "your-openrouter-key"

bootstrap:
  default_repository: true
  default_admin: false
```

## Environment variable overrides

Many config values can be set via environment variables. Env vars take precedence over YAML. The API reads these at startup:

| Env var | Config path | Description |
|---------|------------|-------------|
| `SERPER_API_KEY` | `providers.search.serper.api_key` | Serper web search key |
| `OPENROUTER_API_KEY` | `providers.ai.openrouter.api_key` | OpenRouter LLM key |
| `OLLAMA_API_KEY` | `providers.ai.ollama_cloud.api_key` | Ollama Cloud key |
| `OLLAMA_BASE_URL` | `providers.ai.ollama.base_url` | Local Ollama endpoint |
| `OPENALEX_EMAIL` | `providers.search.openalex.email` | OpenAlex polite-pool email |
| `UNPAYWALL_EMAIL` | `providers.resolution.unpaywall.email` | Unpaywall DOI email |
| `OKT_FETCH_IMPERSONATE` | `providers.resolution.tls.impersonate` | TLS impersonation profile |
| `FLARESOLVERR_URL` | `providers.resolution.flaresolverr.url` | FlareSolverr endpoint |
| `FLARESOLVERR_ENDPOINTS` | `providers.resolution.flaresolverr.endpoints` | Comma-separated endpoints |
| `FLARESOLVERR_MAX_CONCURRENCY` | `providers.resolution.flaresolverr.max_concurrency` | Max in-flight calls |
| `QDRANT_HOST` | `providers.qdrant.host` | Qdrant host |
| `QDRANT_PORT` | `providers.qdrant.port` | Qdrant gRPC port |
| `QDRANT_API_KEY` | `providers.qdrant.api_key` | Qdrant API key |
| `REGISTRY_URL` | `providers.registry.url` | Knowledge Registry URL |
| `REGISTRY_AUTH_MODE` | `providers.registry.auth_mode` | Registry auth mode |
| `REGISTRY_API_KEY` | `providers.registry.api_key` | Registry write key |
| `REGISTRY_READ_API_KEY` | `providers.registry.read_api_key` | Registry read key |
| `AUTH_JWTSECRET` | `auth.jwt_secret` | JWT signing secret |
| `OKT_OAUTH_ISSUER` | `oauth.issuer` | OAuth 2.1 issuer URL |

---

## Sections

### `server`

HTTP server settings.

```yaml
server:
  port: 8080               # Listen port
  read_timeout: 15s         # HTTP read timeout
  write_timeout: 15s        # HTTP write timeout
```

### `databases`

Named Postgres database registry. `default` is always required. Each entry connects to a Postgres instance and carries the full `okt_system` + `okt_repository` schema.

```yaml
databases:
  default:
    host: localhost
    port: 5432
    user: okt
    password: okt_dev
    name: okt
    ssl_mode: disable       # disable | require | verify-ca | verify-full
    max_conns: 200          # Connection pool size
  tasks:                    # Dedicated River task queue database
    host: localhost
    port: 5432
    user: okt
    password: okt_dev
    name: okt_tasks
    ssl_mode: disable
    max_conns: 200
  # Per-tenant databases (optional):
  # iso_8f3a:
  #   host: tenant-pg.internal
  #   port: 5432
  #   ...
```

### `system`

Which named database carries the system tables (users, sessions, casbin, repositories). Empty falls back to `default`.

```yaml
system:
  database: default
```

### `isolation`

Per-repository data isolation controls.

```yaml
isolation:
  default_database: default           # Where new repos land by default
  allowed_databases: []               # Picker allow-list (empty = closed)
  # - iso_8f3a                        # Add names to open the picker
```

### `task`

River task queue configuration. This is a **top-level** key — do not nest it under another section.

```yaml
task:
  database: tasks                     # Named database for River
  job_timeout: 4h                     # Wall-clock cap per job (0s = unlimited)
  heartbeat_interval: 1m              # Worker heartbeat cadence
  heartbeat_timeout: 10m              # Stale heartbeat threshold
  rescue_on_startup: true             # Re-queue orphaned jobs on boot
  refresh_concept_relations_interval: 10m
  queues:                             # Worker counts per job kind
    retrieve_source: 20               # Capped by FlareSolverr pool
    source_decomposition: 100         # LLM-heavy
    embed_facts: 50
    deduplicate_facts: 50
    extract_concepts: 100             # LLM-heavy
    refine_concepts: 100
    embed_concepts: 50
    cleanup_facts: 50
    summarize_concepts: 100           # LLM-heavy
    synthesize_concept: 100           # LLM-heavy
    refresh_concept_relations: 50
    annotate_report: 50
    migrate_context: 50
    contribute_source: 50
    pull_all_from_registry: 50
```

### `auth`

JWT session authentication.

```yaml
auth:
  jwt_secret: "change-me-in-production"   # HMAC secret for JWT signing
  token_ttl: 24h                           # Session token lifetime
```

### `oauth`

OAuth 2.1 authorization server (for MCP clients). Access tokens are HS256 JWTs signed with `auth.jwt_secret`.

```yaml
oauth:
  issuer: ""                      # MUST override in production (e.g. "https://okt.example.com")
  access_token_ttl: 15m           # Short-lived access JWT
  refresh_token_ttl: 720h         # 30 days
  auth_code_ttl: 10m              # Single-use authorization codes
```

### `providers.search`

Web and academic search providers.

```yaml
providers:
  search:
    provider: "serper"            # Default search provider
    serper:
      api_key: ""                 # https://serper.dev
    openalex:
      email: ""                   # Polite-pool email for higher rate limits
```

### `providers.resolution`

Source fetching and resolution chain. Providers self-disable when their config is empty.

```yaml
providers:
  resolution:
    fetch:
      enabled: true
      user_agent: "Mozilla/5.0 ..."
      timeout: 60s
      retry:
        max_attempts: 3           # Including first attempt
        base_delay: 2s
        max_delay: 15s
    unpaywall:
      email: ""                   # https://unpaywall.org — email is the API key
    tls:
      impersonate: ""             # e.g. "chrome_124" — empty disables
      timeout: 30s
    flaresolverr:
      url: ""                     # Single endpoint (env: FLARESOLVERR_URL)
      endpoints: []               # Multi-endpoint pool
      timeout: 60s
      max_concurrency: 0          # 0 = no cap; set to ~number of containers
    host_overrides: {}            # Static host → provider-id map
    chain: ""                     # Comma-separated provider order override
```

### `providers.decomposition`

Source decomposition (fact extraction, concept extraction, image extraction).

```yaml
providers:
  decomposition:
    chunking:
      chunk_size: 2000            # Characters per chunk
      chunk_overlap: 200
    fact_extraction:
      provider: "openrouter"
      model: "google/gemma-4-31b-it"
      concurrency: 4              # Parallel chunks per source
    image_extraction:
      enabled: true
      provider: "ollama_cloud"
      model: "gemma4:31b-cloud"   # Must be multimodal/vision
      max_image_bytes: 5242880    # 5 MB
      max_images_per_source: 20
      concurrency: 4
    concept_extraction:
      enabled: true
      provider: "openrouter"
      model: "google/gemma-4-31b-it"
      fact_batch_size: 10         # Facts per LLM call
      concurrency: 4
```

### `providers.embedding`

Vector embedding provider. The model and dimensions must match the Qdrant collection config.

```yaml
providers:
  embedding:
    provider: "openrouter"
    model: "google/gemini-embedding-2"
    dimensions: 3072
    # Free local alternative (requires Qdrant collection rebuild):
    # provider: "ollama"
    # model: "qwen3-embedding"
    # dimensions: 1024
```

### `providers.qdrant`

Vector store connection.

```yaml
providers:
  qdrant:
    host: localhost
    port: 6334                    # gRPC port (REST is port-1)
    api_key: ""                   # Empty when Qdrant has no auth
    collection: "okt_facts"
    concept_collection: "okt_concepts"
    allow_recreate: false         # true in dev to rebuild collections
```

### `providers.dedup`

Cross-source deduplication thresholds.

```yaml
providers:
  dedup:
    threshold: 0.94               # Cosine similarity above which facts are duplicates
    catchup_max_age: 168h         # Reap stuck facts older than this
```

### `providers.reports`

Report auto-annotation (autocitation).

```yaml
providers:
  reports:
    enabled: true
    similarity_threshold: 0.7     # Lower than dedup — we want "related", not "duplicate"
    max_facts_per_sentence: 5
    min_sentence_runes: 40
    posture_classifier:
      enabled: true               # Labels: related | supports | contradicts
      provider: "openrouter"
      model: "google/gemma-4-31b-it"
      batch_size: 8
      max_concurrent: 4
      max_tokens: 800
```

### `providers.storage`

File storage for source assets (images, PDF bodies).

```yaml
providers:
  storage:
    backend: filesystem           # Only "filesystem" implemented today
    filesystem:
      root: var/source_assets
    # s3:                         # Reserved for future use
    #   bucket: ""
    #   region: ""
    #   endpoint: ""
```

### `providers.registry`

Knowledge Registry connection (pre-decomposed source cache).

```yaml
providers:
  registry:
    url: "https://registry.openktree.com"  # Leave empty to disable
    auth_mode: ""                            # none | bearer | hmac
    api_key: ""                              # Write key
    read_api_key: ""                         # Read key (falls back to api_key)
    allowed_models: []                       # ["*"] to allow all
```

### `providers.summarization`

Concept summarization (incremental summary slices).

```yaml
providers:
  summarization:
    enabled: false               # Off by default — enable after bootstrapping facts
    provider: "openrouter"
    model: "google/gemma-4-31b-it"
    batch_size: 20               # Facts per summary slice
    max_concepts_per_run: 40
    lock_staleness: 2h
    max_tokens: 600              # ~450 words per slice
```

### `providers.refinement`

Concept refinement (resolve unresolved concept candidates).

```yaml
providers:
  refinement:
    enabled: false               # Off by default
    provider: "openrouter"
    model: "google/gemma-4-31b-it"
    max_candidates_per_run: 40
    prune_threshold: 5
    max_tokens: 400
    max_concurrency: 5
```

### `providers.synthesis`

Concept synthesis ("definitions" — the authoritative crystallized knowledge).

```yaml
providers:
  synthesis:
    enabled: true
    provider: "openrouter"
    model: "deepseek/deepseek-v4-flash:turbo"
    image_picker_model: "google/gemma-4-31b-it"
    max_tokens: 10000
    thinking_level: "low"        # low | medium | high
    max_images: 10
    max_image_candidates: 50
    max_related_concepts: 10
    max_related_syntheses: 3
```

### `providers.ai`

LLM provider connections and model catalog.

```yaml
providers:
  ai:
    ollama:
      base_url: ""               # Local Ollama endpoint (e.g. "http://localhost:11434")
    ollama_cloud:
      api_key: ""                # Ollama Cloud API key
    openrouter:
      api_key: ""
      embed_batch_size: 32       # Inputs per embedding POST
    models:                      # Catalog with rate limits and cost
      - id: "google/gemma-4-31b-it"
        provider: "openrouter"
        input_cost_per_1m: 0.12
        output_cost_per_1m: 0.4
        rate_limit_rpm: 500
      - id: "google/gemini-embedding-2"
        provider: "openrouter"
        input_cost_per_1m: 0.0
        output_cost_per_1m: 0.0
        rate_limit_rpm: 500
```

### `bootstrap`

One-time startup data creation. Each step is idempotent and only acts when its target table is empty.

```yaml
bootstrap:
  default_repository: true       # Create a starter repository on first boot
  default_admin: false           # Seed a sysadmin (credentials via env vars)
  # OKT_BOOTSTRAP_DEFAULT_ADMIN_EMAIL: admin@example.com
  # OKT_BOOTSTRAP_DEFAULT_ADMIN_PASSWORD: change-me
  # OKT_BOOTSTRAP_DEFAULT_ADMIN_DISPLAY_NAME: "Default Admin"
```

### `repository_presets`

The repository "types" offered by the create-repository UI. Each preset bundles a provider set and context allow-list.

```yaml
repository_presets:
  - id: general
    label: General
    description: "All providers enabled, full context vocabulary."
    providers:
      search: ["serper", "openalex"]
      resolution: ["fetch", "unpaywall", "tls", "flaresolverr"]
    contexts: ["all"]
  - id: scientific
    label: Scientific
    description: "Academic search + open-access resolution."
    providers:
      search: ["openalex"]
      resolution: ["fetch", "unpaywall", "tls"]
    contexts: ["Biomolecule", "drug", "gene", "protein", ...]
  - id: enterprise
    label: Enterprise
    description: "Web search + plain fetch, custom contexts."
    providers:
      search: ["serper"]
      resolution: ["fetch"]
    contexts: []
    custom_contexts: ["Product", "Application", "Role"]

default_repository_preset: general
```
