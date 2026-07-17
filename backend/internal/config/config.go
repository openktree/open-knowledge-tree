package config

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/openktree/open-knowledge-tree/backend/configs"
	"github.com/spf13/viper"
)

// Config is the top-level configuration object. It is loaded once at
// startup from configs/config.default.yaml + configs/config.local.yaml +
// .env overrides, and is then read-only.
//
// The new shape supports multiple Postgres databases by name: see
// `Databases`. The legacy single-database fields (`Database`,
// `TaskConfig.Host/Port/...`) are still parsed for backward
// compatibility; `Load()` synthesizes a `Databases["default"]` (and a
// `Databases["tasks"]` when task.* legacy fields are present) and
// logs a one-time deprecation warning.
type Config struct {
	Server    ServerConfig              `mapstructure:"server"`
	Database  DatabaseConfig            `mapstructure:"database"`
	Databases map[string]DatabaseConfig `mapstructure:"databases"`
	Task      TaskConfig                `mapstructure:"task"`
	System    SystemConfig              `mapstructure:"system"`
	Isolation IsolationConfig           `mapstructure:"isolation"`
	Auth      AuthConfig                `mapstructure:"auth"`
	OAuth     OAuthConfig               `mapstructure:"oauth"`
	Providers ProvidersConfig           `mapstructure:"providers"`
	Bootstrap BootstrapConfig           `mapstructure:"bootstrap"`

	// RepositoryPresets is the catalog of repository "types" the
	// create-repository UI offers (e.g. Scientific, General,
	// Enterprise). Each preset carries a curated set of providers to
	// enable and a curated context allow-list (a subset of the
	// embedded context vocabulary labels, or the special value "all"
	// to mean the full embedded set). Presets are config-driven so an operator can tune or add
	// them without a code change. DefaultRepositoryPreset names the
	// preset used when the create body omits `preset`.
	RepositoryPresets       []RepositoryPreset `mapstructure:"repository_presets"`
	DefaultRepositoryPreset  string             `mapstructure:"default_repository_preset"`
}

// OAuthConfig configures OKT's built-in OAuth 2.1 authorization
// server. The server is what lets MCP clients (Claude Desktop, etc.)
// connect to the OKT MCP endpoint via Authorization Code + PKCE
// instead of a static API token. The access tokens it issues are
// HS256 JWTs signed with cfg.Auth.JWTSecret (shared with the session
// JWT) so the MCP resource server can validate them statelessly.
//
// Issuer is the externally-resolvable base URL of the OKT instance
// (e.g. "https://okt.example.com"). It is the `iss` claim on access
// tokens and the `issuer` field in the OAuth metadata. Empty falls
// back to "http://localhost:<server.port>" for local dev; production
// deployments MUST override it via OKT_OAUTH_ISSUER or the YAML.
//
// The TTLs default to 15m (access), 30d (refresh), 10m (auth code)
// when zero. The defaults match the OAuth 2.1 best practice and the
// spec-recommended authorization-code lifetime.
type OAuthConfig struct {
	Issuer          string        `mapstructure:"issuer"`
	AccessTokenTTL  time.Duration `mapstructure:"access_token_ttl"`
	RefreshTokenTTL time.Duration `mapstructure:"refresh_token_ttl"`
	AuthCodeTTL     time.Duration `mapstructure:"auth_code_ttl"`
}

// SystemConfig identifies which database carries the system tables
// (users, sessions, casbin_rule, the repositories registry). When
// `Database` is empty, the registry falls back to "default". The
// named database must exist in `Databases`.
type SystemConfig struct {
	Database string `mapstructure:"database"`
}

// IsolationConfig declares the database layout for per-repository
// data.
//
// `DefaultDatabase` is the name (in `Databases`) of the database a
// new repository is created in when the picker doesn't pick anything
// (or when the caller lacks picker permission). Empty falls back to
// "default".
//
// `AllowedDatabases` is the allow-list the picker validates against.
// Every entry must be a key in `Databases`; every name in
// `Databases` is automatically a valid pick (no need to list "default"
// if you want it available — but listing it makes the picker
// explicit). When the slice is empty, the picker is closed (no
// caller may select a non-default database) and every new repository
// lands in `DefaultDatabase`.
//
// The "tier" — shared, isolated, sovereign — is the value stored in
// the repositories row when the database is picked; the picker UI
// uses it to render the warning callout. The server does not
// enforce the tier; it just records what the caller picked so a
// later "tier up" can move the data.
type IsolationConfig struct {
	DefaultDatabase  string   `mapstructure:"default_database"`
	AllowedDatabases []string `mapstructure:"allowed_databases"`
}

// TierForDatabaseName returns the tier string the server should
// store in `repositories.tier` when a new repository is created in
// the given database. The current rule is a simple mapping: the
// default database is "shared", everything else is "isolated". A
// future "sovereign" tier (dedicated cluster, region pinning) can
// extend this with a config key, e.g.
// `isolation.sovereign_databases: [eu_sovereign, ...]`.
func (i IsolationConfig) TierForDatabaseName(name string) string {
	if name == "" || name == i.DefaultDatabase {
		return "shared"
	}
	return "isolated"
}

// TierFor is the package-level form of
// IsolationConfig.TierForDatabaseName. It exists for callers
// (e.g. HTTP handlers) that have a *Config in hand and don't want
// to reach into the Isolation field at every call site.
func TierFor(i IsolationConfig, name string) string {
	return i.TierForDatabaseName(name)
}

// BootstrapConfig controls one-time startup data creation. The
// bootstrap package runs these checks after schema apply and RBAC
// seeding, and only acts when the relevant table is empty, so the
// settings are safe to leave enabled in production.
//
// The two steps are independent and may be enabled/disabled
// separately. Both are no-ops once their target table is
// non-empty, so they're safe to leave on across restarts.
type BootstrapConfig struct {
	// DefaultRepository, when true, makes the app create a single
	// starter repository the first time it boots against an empty
	// database. The repository is owned by the calling user when
	// the lazy path in GET /repositories runs (see
	// internal/api/handler/repository.go), so the first user to
	// hit the API naturally becomes the owner. The startup path
	// (EnsureDefaultRepository at boot) is a no-op when no user
	// exists yet; the lazy path picks up the slack.
	DefaultRepository bool `mapstructure:"default_repository"`

	// DefaultAdmin, when true, seeds a system administrator the
	// first time the app boots against an empty users table. The
	// email, password, and display name are read from the
	// OKT_BOOTSTRAP_DEFAULT_ADMIN_{EMAIL,PASSWORD,DISPLAY_NAME}
	// env vars; the password is treated as a secret and not
	// exposed in the YAML schema. The step is skipped if any of
	// the three env vars is missing or if the users table is
	// non-empty. Once the admin is seeded, disabling the flag
	// has no effect (the user persists); the env vars may be
	// removed or rotated safely.
	DefaultAdmin bool `mapstructure:"default_admin"`
}

// DefaultAdminEnv returns the admin credentials resolved from the
// OKT_BOOTSTRAP_DEFAULT_ADMIN_* env vars. Returns ("", "", "", false)
// when the env vars are absent or the bootstrap flag is disabled;
// callers must treat the false return as "do nothing". Reading
// from env vars (instead of mapstructure) keeps the password out
// of the YAML config and lets operators rotate it without
// touching a config file.
func (b BootstrapConfig) DefaultAdminEnv() (email, password, displayName string, ok bool) {
	if !b.DefaultAdmin {
		return "", "", "", false
	}
	email = strings.TrimSpace(os.Getenv("OKT_BOOTSTRAP_DEFAULT_ADMIN_EMAIL"))
	password = os.Getenv("OKT_BOOTSTRAP_DEFAULT_ADMIN_PASSWORD")
	displayName = strings.TrimSpace(os.Getenv("OKT_BOOTSTRAP_DEFAULT_ADMIN_DISPLAY_NAME"))
	if email == "" || password == "" {
		return "", "", "", false
	}
	if displayName == "" {
		displayName = "Default Admin"
	}
	return email, password, displayName, true
}

// RepositoryPreset is one entry in the `repository_presets` config
// list. A preset bundles a curated provider set + context allow-list
// that the create-repository UI applies in one click.
//
// ID is the stable slug the create body sends ("scientific",
// "general", "enterprise"); Label is the human string the UI shows.
// Providers is a map of kind ("search"|"resolution") → list of
// provider ids to seed enabled; an absent kind means "no providers
// of that kind enabled" (the admin adds them via the UI). Contexts
// is the curated context allow-list; the special string "all" means
// the full embedded context vocabulary (resolved at creation time);
// an empty/absent list means "no contexts allowed" (the admin fills
// custom ones — the Enterprise pattern). CustomContexts lists
// non-standard labels to also seed with is_custom=TRUE (e.g.
// "Product", "Application"); each may carry an optional
// description via the parallel CustomContextDescriptions map
// (label → description).
type RepositoryPreset struct {
	ID          string   `mapstructure:"id"`
	Label       string   `mapstructure:"label"`
	Description string   `mapstructure:"description"`
	Providers   map[string][]string `mapstructure:"providers"`
	Contexts    []string `mapstructure:"contexts"`
	CustomContexts            []string            `mapstructure:"custom_contexts"`
	CustomContextDescriptions map[string]string   `mapstructure:"custom_context_descriptions"`
}

// PresetByID returns the preset with the given id, or nil when no
// preset matches. Used by the create handler to resolve the body's
// `preset` field; the caller falls back to DefaultRepositoryPreset
// when the field is empty.
func (c *Config) PresetByID(id string) *RepositoryPreset {
	for i := range c.RepositoryPresets {
		if c.RepositoryPresets[i].ID == id {
			return &c.RepositoryPresets[i]
		}
	}
	return nil
}

// DefaultPreset returns the preset named by DefaultRepositoryPreset,
// or nil when that field is empty or names no preset. Used by the
// create handler when the body omits `preset`.
func (c *Config) DefaultPreset() *RepositoryPreset {
	if c.DefaultRepositoryPreset == "" {
		return nil
	}
	return c.PresetByID(c.DefaultRepositoryPreset)
}

type ProvidersConfig struct {
	Search        SearchProvidersConfig        `mapstructure:"search"`
	Resolution    ResolutionProvidersConfig    `mapstructure:"resolution"`
	AI            AIProvidersConfig            `mapstructure:"ai"`
	Decomposition DecompositionProvidersConfig `mapstructure:"decomposition"`
	Embedding     EmbeddingConfig              `mapstructure:"embedding"`
	Qdrant        QdrantConfig                 `mapstructure:"qdrant"`
	Dedup         DedupConfig                  `mapstructure:"dedup"`
	Storage       StorageConfig                `mapstructure:"storage"`
	Summarization SummarizationConfig          `mapstructure:"summarization"`
	Refinement    RefinementConfig              `mapstructure:"refinement"`
	Synthesis     SynthesisConfig              `mapstructure:"synthesis"`
	Reports       ReportsConfig                `mapstructure:"reports"`
	Registry      RegistryConfig               `mapstructure:"registry"`
	Registries    []RegistryConfig             `mapstructure:"registries"`
}

// RegistryConfig configures the connection to the Knowledge Registry
// service. The registry stores pre-decomposed sources (facts, concepts,
// embeddings) in S3/R2 and provides a lookup API that the retrieve_source
// worker checks before fetching.
//
// `URL` is the base URL of the registry API (e.g. "http://registry:8081").
// When empty, the registry integration is disabled and the worker always
// falls through to the normal fetch + decompose path.
//
// `AuthMode` selects the authentication scheme used to talk to the
// registry: "none" (public, no auth headers) or "bearer" (API key sent
// as Authorization: Bearer). When empty, defaults to "none".
//
// `APIKey` is the write key (push operations). Empty means push is
// disabled.
//
// `ReadAPIKey` is an optional separate read key (search, pull). When
// empty, falls back to APIKey. This lets an operator issue a read-only
// key for public consumption while keeping the write key restricted.
//
// `AllowedModels` is a list of decomposition model IDs the worker is
// allowed to import from the registry. When empty, no decompositions
// are imported (the worker only checks for source metadata). Set to
// ["*"] to allow all models.
type RegistryConfig struct {
	ID            string   `mapstructure:"id"`
	URL           string   `mapstructure:"url"`
	AuthMode      string   `mapstructure:"auth_mode"`
	APIKey        string   `mapstructure:"api_key"`
	ReadAPIKey    string   `mapstructure:"read_api_key"`
	AllowedModels []string `mapstructure:"allowed_models"`
}

// ResolveRegistries returns the effective list of configured
// registries. When the operator only sets the legacy single
// `providers.registry` block (no `registries[]` list), it is
// synthesized as a one-element list with id "default" so the rest of
// the codebase can always iterate over `registries`. When the
// `registries[]` list is set, entries with an empty id are assigned
// "default" for the first one and "registry-N" for the rest so every
// entry has a stable, selectable id. Entries with an empty URL are
// dropped (a disabled registry is expressed by omitting it).
func (p ProvidersConfig) ResolveRegistries() []RegistryConfig {
	out := make([]RegistryConfig, 0, max(len(p.Registries), 1))
	for i, r := range p.Registries {
		if r.URL == "" {
			continue
		}
		if r.ID == "" {
			if i == 0 {
				r.ID = "default"
			} else {
				r.ID = "registry-" + strconv.Itoa(i+1)
			}
		}
		out = append(out, r)
	}
	if len(out) > 0 {
		return out
	}
	// Legacy single-block fallback.
	if p.Registry.URL != "" {
		r := p.Registry
		if r.ID == "" {
			r.ID = "default"
		}
		return []RegistryConfig{r}
	}
	return out
}

// RegistryIDs returns the ids of every configured registry, in order.
// Empty when no registry is configured.
func (p ProvidersConfig) RegistryIDs() []string {
	regs := p.ResolveRegistries()
	ids := make([]string, 0, len(regs))
	for _, r := range regs {
		ids = append(ids, r.ID)
	}
	return ids
}

// RegistryByID returns the configured registry with the given id and
// ok=false when no such registry exists.
func (p ProvidersConfig) RegistryByID(id string) (RegistryConfig, bool) {
	for _, r := range p.ResolveRegistries() {
		if r.ID == id {
			return r, true
		}
	}
	return RegistryConfig{}, false
}

// AnyRegistryConfigured reports whether at least one registry is
// configured (has a non-empty URL). This is the global gate; the
// per-repo `registry_enabled` flag further restricts a single repo.
func (p ProvidersConfig) AnyRegistryConfigured() bool {
	return len(p.ResolveRegistries()) > 0
}

// StorageConfig configures the file storage backend used to persist
// source images (inline and PDF page renders) and full PDF source
// bodies. The backend is pluggable: `Backend` selects the
// implementation ("filesystem" today; "s3" reserved for the future
// cloud/CDN provider). Each backend's settings live under its own
// sub-key so an operator can switch backends by changing one field.
//
// Files are keyed per-repository/per-source under
// `repositories/{repoID}/sources/{sourceID}/...` so the layout is
// easy to enumerate and clean up; future object-store backends can
// flatten or shard the prefix without changing the key scheme.
type StorageConfig struct {
	Backend    string           `mapstructure:"backend"` // "filesystem" (default), "s3" (future)
	Filesystem FilesystemConfig `mapstructure:"filesystem"`
	S3         S3Config         `mapstructure:"s3"`
}

// FilesystemConfig configures the local-disk storage backend. `Root`
// is the directory the backend writes to; it is created at boot when
// it doesn't exist. Paths are resolved relative to the process
// working directory unless absolute. The directory must be writable
// by the api/task process and persistent across restarts (mount a
// docker volume in production).
type FilesystemConfig struct {
	Root string `mapstructure:"root"`
}

// S3Config is reserved for the future cloud/CDN storage provider.
// It is parsed but unused today; the `s3.go` stub in
// `internal/providers/storage` returns ErrNotImplemented until the
// implementation lands.
type S3Config struct {
	Bucket   string `mapstructure:"bucket"`
	Region   string `mapstructure:"region"`
	Endpoint string `mapstructure:"endpoint"`
}

// EmbeddingConfig configures the embedding provider used by the
// embed_facts task to vectorize extracted facts into Qdrant. The
// provider name must match a key in the `ai` block (the same
// provider instances are reused); the model and dimensions must be
// consistent with the Qdrant collection's vector size, otherwise
// EnsureCollection fails at boot (or drops+recreates when
// `qdrant.allow_recreate` is true, dev-only).
type EmbeddingConfig struct {
	Provider   string `mapstructure:"provider"`   // "openrouter" (default) | "ollama"
	Model      string `mapstructure:"model"`      // "google/gemini-embedding-2" | "qwen3-embedding"
	Dimensions int    `mapstructure:"dimensions"` // 3072 (gemini/text-embedding-3-large) | 1024 (qwen3)
}

// QdrantConfig configures the Qdrant vector store client. Qdrant
// is a dumb vector index: payloads carry `{repository_id, status}`
// only — no fact text, no source_id. Postgres is the single source
// of truth for everything except the vector. The collection is
// shared (single `okt_facts` collection) and filtered by
// `repository_id` at query time; tier isolation lives in Postgres.
type QdrantConfig struct {
	Host       string `mapstructure:"host"`
	Port       int    `mapstructure:"port"` // 6334 gRPC
	APIKey     string `mapstructure:"api_key"`
	Collection string `mapstructure:"collection"` // "okt_facts"
	// ConceptCollection is the Qdrant collection used by the
	// embed_concepts worker to store concept vectors (canonical_name
	// + " " + context). Separate from the facts collection so concept
	// searches don't pay the cost of scanning fact vectors, and so a
	// dimension change on one collection doesn't force re-embedding
	// the other. Defaults to "okt_concepts".
	ConceptCollection string `mapstructure:"concept_collection"`
	AllowRecreate     bool   `mapstructure:"allow_recreate"` // false by default; dev-only dimension-mismatch drop+recreate
}

// DedupConfig configures the per-repository cross-source dedup
// pipeline. Threshold is the cosine similarity score above which
// two facts are considered duplicates (0.94 by default).
// CatchupMaxAge is the age beyond which stuck `to_delete`/`new`
// facts are reaped by the daily `fact_catchup` periodic job; it is
// parsed as a Go time.Duration string (e.g. "168h").
type DedupConfig struct {
	Threshold     float64 `mapstructure:"threshold"`       // 0.94
	CatchupMaxAge string  `mapstructure:"catchup_max_age"` // "168h"
}

// CatchupMaxAgeDuration parses DedupConfig.CatchupMaxAge as a
// time.Duration, returning the default (168h) when the field is
// empty or unparseable. Callers (the catchup worker) use this to
// avoid re-implementing the fallback at every call site.
func (d DedupConfig) CatchupMaxAgeDuration() time.Duration {
	if d.CatchupMaxAge == "" {
		return 168 * time.Hour
	}
	if dur, err := time.ParseDuration(d.CatchupMaxAge); err == nil {
		return dur
	}
	return 168 * time.Hour
}

type DecompositionProvidersConfig struct {
	Chunking          DecompositionChunkingConfig `mapstructure:"chunking"`
	FactExtraction    DecompositionFactConfig     `mapstructure:"fact_extraction"`
	ImageExtraction   DecompositionImageConfig    `mapstructure:"image_extraction"`
	ConceptExtraction DecompositionConceptConfig  `mapstructure:"concept_extraction"`
}

// DecompositionImageConfig configures the multimodal image fact
// extractor. It runs after the text-chunk loop inside the same
// source_decomposition job. Each image attached to a source is sent
// to the configured multimodal model (together with the source URL,
// title, and the image's alt text) and the model returns fact-oriented
// descriptions that become image facts (facts with a non-null
// image_url). When Enabled is false or the provider/model is not
// configured, image extraction is a no-op and the worker skips it
// gracefully.
type DecompositionImageConfig struct {
	Enabled            bool   `mapstructure:"enabled"`
	Provider           string `mapstructure:"provider"`
	Model              string `mapstructure:"model"`
	MaxImageBytes      int64  `mapstructure:"max_image_bytes"`
	MaxImagesPerSource int    `mapstructure:"max_images_per_source"`
	Concurrency        int    `mapstructure:"concurrency"`
}

// ConcurrencyOr returns the configured image-extraction concurrency or
// the default (4) when unset or non-positive. The value caps how many
// images are sent to the multimodal model concurrently within one
// source_decomposition job; the DB persistence phase stays serial.
func (c DecompositionImageConfig) ConcurrencyOr(def int) int {
	if c.Concurrency > 0 {
		return c.Concurrency
	}
	if def > 0 {
		return def
	}
	return 4
}

type DecompositionChunkingConfig struct {
	Provider     string `mapstructure:"provider"`
	ChunkSize    int    `mapstructure:"chunk_size"`
	ChunkOverlap int    `mapstructure:"chunk_overlap"`
}

type DecompositionFactConfig struct {
	Provider    string `mapstructure:"provider"`
	Model       string `mapstructure:"model"`
	Concurrency int    `mapstructure:"concurrency"`
}

// ConcurrencyOr returns the configured fact-extraction concurrency or
// the default (4) when unset or non-positive. The value caps how many
// chunks are sent to the model concurrently within one
// source_decomposition job; the DB persistence phase stays serial.
func (c DecompositionFactConfig) ConcurrencyOr(def int) int {
	if c.Concurrency > 0 {
		return c.Concurrency
	}
	if def > 0 {
		return def
	}
	return 4
}

// DecompositionConceptConfig configures the concept-extraction
// provider. The provider is called once per batch of stable facts
// (after dedup) and returns a list of (fact_index, concept, context,
// seed_aliases) tuples; the context is drawn from the embedded
// context vocabulary the worker passes into the prompt. When Enabled
// is false or the provider/model is not configured, the
// extract_concepts worker logs "not configured" and is a no-op (the
// rest of the pipeline still runs).
//
// One round fetches Concurrency × FactBatchSize facts (one parallel
// wave), splits them into Concurrency chunks of FactBatchSize facts
// each, fans the chunks out at Concurrency, persists the results
// serially, then re-fetches. FactBatchSize caps how many facts are
// sent to the LLM in a single call (token/call-cost tradeoff);
// Concurrency caps how many of those calls run in parallel
// (throughput vs. rate limits). There is no separate fetch-batch
// knob — the wave size is derived so every fetched fact is in a
// chunk that runs immediately, with no straggler waiting for a
// semaphore slot.
type DecompositionConceptConfig struct {
	Enabled       bool   `mapstructure:"enabled"`
	Provider      string `mapstructure:"provider"`
	Model         string `mapstructure:"model"`
	FactBatchSize int    `mapstructure:"fact_batch_size"`
	Concurrency   int    `mapstructure:"concurrency"`
}

// FactBatchSizeOr returns the configured fact batch size or the
// default (10) when unset or non-positive. The value caps how many
// facts are sent to the concept model in a single LLM call; each
// BatchSize round is split into ceil(BatchSize/FactBatchSize) calls
// that fan out at ConcurrencyOr. Raising it lowers the per-fact
// overhead (one ontology prefix per call instead of one per fact)
// at the cost of a larger single response.
func (c DecompositionConceptConfig) FactBatchSizeOr(def int) int {
	if c.FactBatchSize > 0 {
		return c.FactBatchSize
	}
	if def > 0 {
		return def
	}
	return 10
}

// ConcurrencyOr returns the configured concept-extraction concurrency or
// the default (4) when unset or non-positive. The value caps how many
// facts are sent to the concept model concurrently within one batch of
// an extract_concepts pass; the DB persistence phase (concept creation +
// fact linking) stays serial per fact in input order.
func (c DecompositionConceptConfig) ConcurrencyOr(def int) int {
	if c.Concurrency > 0 {
		return c.Concurrency
	}
	if def > 0 {
		return def
	}
	return 4
}

// RefinementConfig configures the concept-refinement provider that
// resolves unresolved concept candidates: proposes a full formal
// canonical name, known aliases to add, and aliases to prune. Runs
// once per unresolved candidate (genuinely new concepts only; resolved
// candidates route via cache, matched candidates route via pre-LLM DB
// queries). The refine_concepts task fans out from extract_concepts
// and runs before summarize_concepts so the summarizer sees the
// final canonical names. When Enabled is false or the provider/model
// is not configured, the refine_concepts worker is a no-op.
type RefinementConfig struct {
	Enabled              bool   `mapstructure:"enabled"`
	Provider             string `mapstructure:"provider"`
	Model                string `mapstructure:"model"`
	MaxCandidatesPerRun  int    `mapstructure:"max_candidates_per_run"`
	PruneThreshold       int    `mapstructure:"prune_threshold"`
	MaxTokens            int    `mapstructure:"max_tokens"`
	MaxConcurrency       int    `mapstructure:"max_concurrency"`
}

// MaxTokensOr returns the configured MaxTokens or the default (400)
// when zero/negative.
func (r RefinementConfig) MaxTokensOr(def int) int {
	if r.MaxTokens > 0 {
		return r.MaxTokens
	}
	return def
}

// PruneThresholdOr returns the configured PruneThreshold or the
// default (5) when zero/negative. The pruning gate: re-prune an
// established concept's aliases only when >= X new aliases have
// accumulated since the last refinement.
func (r RefinementConfig) PruneThresholdOr(def int) int {
	if r.PruneThreshold > 0 {
		return r.PruneThreshold
	}
	return def
}

// MaxConcurrencyOr returns the configured MaxConcurrency or the
// default (5) when zero/negative. Bounds the number of candidates
// processed concurrently within a single RefineConcepts job — the
// main wall-time knob since each LLM-needing candidate makes its own
// chat call.
func (r RefinementConfig) MaxConcurrencyOr(def int) int {
	if r.MaxConcurrency > 0 {
		return r.MaxConcurrency
	}
	return def
}

// SummarizationConfig configures the concept summarization
// provider. The summarization task (tasks.SummarizeConcepts) fans
// out from extract_concepts in parallel with embed_concepts; each
// task chunk processes at most MaxConceptsPerRun concepts so a
// large repo doesn't blow the River JobTimeout on a single job.
//
// BatchSize is the per-slice fact count: when an open summary
// reaches BatchSize covered facts it freezes (is_complete = TRUE)
// and the next batch of facts seeds a new open summary. LockStaleness
// is the Postgres interval after which a held per-concept
// summarizing_at lock is reclaimable by the next worker run (so a
// crashed worker doesn't wedge the concept forever).
//
// When Enabled is false or the provider/model is not configured,
// the summarize_concepts worker logs "not configured" and is a
// no-op (extract_concepts does not enqueue it).
//
// MaxTokens caps the LLM's output length per summary slice, so a
// 20-fact slice stays a bounded read instead of an unbounded wall
// of text. The prompt separately nudges the model toward a concise
// (~400-500 word) summary; MaxTokens is the hard backstop. Default
// 600 (roughly 450 words) is the medium budget.
type SummarizationConfig struct {
	Enabled           bool          `mapstructure:"enabled"`
	Provider          string        `mapstructure:"provider"`             // ai provider id, e.g. "openrouter"
	Model             string        `mapstructure:"model"`                // chat model id
	BatchSize         int           `mapstructure:"batch_size"`           // facts per summary slice; default 20
	MaxConceptsPerRun int           `mapstructure:"max_concepts_per_run"` // chunk size for fan-out; default 40
	LockStaleness     time.Duration `mapstructure:"lock_staleness"`       // reclaimable-after window; default 2h
	MaxTokens         int           `mapstructure:"max_tokens"`           // per-slice output token cap; default 600
}

// BatchSizeOr returns the configured BatchSize or the default (20)
// when zero/negative. Callers (the worker) use this so a missing
// config value doesn't crash the summarization pass.
func (s SummarizationConfig) BatchSizeOr(def int) int {
	if s.BatchSize > 0 {
		return s.BatchSize
	}
	return def
}

// MaxConceptsPerRunOr returns the configured MaxConceptsPerRun or
// the default (40) when zero/negative.
func (s SummarizationConfig) MaxConceptsPerRunOr(def int) int {
	if s.MaxConceptsPerRun > 0 {
		return s.MaxConceptsPerRun
	}
	return def
}

// LockStalenessOr returns the configured LockStaleness or the
// default (2h) when zero. The worker casts this to a Postgres
// interval string before passing it to ClaimConceptForSummary.
func (s SummarizationConfig) LockStalenessOr(def time.Duration) time.Duration {
	if s.LockStaleness > 0 {
		return s.LockStaleness
	}
	return def
}

// MaxTokensOr returns the configured MaxTokens or the default (600)
// when zero/negative. The worker threads this into the
// SummarizationRequest so the provider can set ChatRequest.MaxTokens
// as a hard cap on each slice's output length.
func (s SummarizationConfig) MaxTokensOr(def int) int {
	if s.MaxTokens > 0 {
		return s.MaxTokens
	}
	return def
}

// SynthesisConfig configures the concept-synthesis provider. The
// synthesize_concept task (chained from summarize_concepts) folds ALL
// of a canonical-name group's summary slices into ONE authoritative
// "definition" row per (repository_id, lower(canonical_name)). Unlike
// SummarizationConfig there is no BatchSize / MaxConceptsPerRun —
// synthesis is one LLM call per group, triggered when a slice in the
// group is written/updated.
//
// ImagePickerModel is the model id for the separate image-picker LLM
// call (a cheaper/faster model is appropriate). When empty, Model is
// used. MaxImages caps how many candidate images the synthesis may
// embed (the synthesis agent may choose 0..MaxImages). MaxImageCandidates
// is the DB-side cap on how many image facts are loaded before the
// picker narrows the pool; when the candidate count is <= MaxImages
// the picker call is skipped and all candidates pass straight through.
//
// When Enabled is false or the provider/model is not configured, the
// synthesize_concept worker is a no-op and summarize_concepts does
// not enqueue it. MaxTokens caps the LLM's output length per
// synthesis (default 1200, roughly 900 words — a definition is
// richer than a single summary slice).
//
// MaxRelatedConcepts (N1) caps how many related concept names with
// per-context shared_fact_counts are loaded as the graph-structure
// block of the synthesis prompt (default 10). MaxRelatedSyntheses
// (N2) caps how many of those N1 also have their existing
// concept_syntheses.content attached verbatim — smaller than N1
// (default 3) because each synthesis adds substantial prompt context.
// N2 is clamped to <= N1 in Load().
type SynthesisConfig struct {
	Enabled             bool   `mapstructure:"enabled"`
	Provider            string `mapstructure:"provider"`              // ai provider id, e.g. "openrouter"
	Model               string `mapstructure:"model"`                 // synthesis chat model id
	ImagePickerModel    string `mapstructure:"image_picker_model"`    // image-picker model id; defaults to Model
	MaxTokens           int    `mapstructure:"max_tokens"`            // synthesis output cap; default 1200
	MaxImages           int    `mapstructure:"max_images"`            // max embedded images; default 10
	MaxImageCandidates  int    `mapstructure:"max_image_candidates"`  // DB cap before picker; default 50
	MaxRelatedConcepts  int    `mapstructure:"max_related_concepts"`  // top N1 related concept names + per-context counts; default 10
	MaxRelatedSyntheses int    `mapstructure:"max_related_syntheses"` // top N2 of N1 with synthesis text attached; default 3
	// ThinkingLevel controls the model's reasoning effort ("low", "medium",
	// "high") when the provider supports it. Only the OpenRouter provider
	// passes it as reasoning_effort today; other providers ignore it. Empty
	// string (default) means "no preference; let the model decide". Set to
	// "low" for synthesis since it's primarily a prose-composition task where
	// extended reasoning chains waste tokens.
	ThinkingLevel string `mapstructure:"thinking_level"`
}

// MaxTokensOr returns the configured MaxTokens or the default (1200)
// when zero/negative.
func (s SynthesisConfig) MaxTokensOr(def int) int {
	if s.MaxTokens > 0 {
		return s.MaxTokens
	}
	return def
}

// MaxImagesOr returns the configured MaxImages or the default (10)
// when zero/negative.
func (s SynthesisConfig) MaxImagesOr(def int) int {
	if s.MaxImages > 0 {
		return s.MaxImages
	}
	return def
}

// MaxImageCandidatesOr returns the configured MaxImageCandidates or
// the default (50) when zero/negative.
func (s SynthesisConfig) MaxImageCandidatesOr(def int) int {
	if s.MaxImageCandidates > 0 {
		return s.MaxImageCandidates
	}
	return def
}

// ImagePickerModelOr returns the configured ImagePickerModel, or
// fallback when empty.
func (s SynthesisConfig) ImagePickerModelOr(fallback string) string {
	if s.ImagePickerModel != "" {
		return s.ImagePickerModel
	}
	return fallback
}

// MaxRelatedConceptsOr returns the configured MaxRelatedConcepts (N1)
// or the default (10) when zero/negative.
func (s SynthesisConfig) MaxRelatedConceptsOr(def int) int {
	if s.MaxRelatedConcepts > 0 {
		return s.MaxRelatedConcepts
	}
	return def
}

// MaxRelatedSynthesesOr returns the configured MaxRelatedSyntheses
// (N2) or the default (3) when zero/negative. It is the caller's
// responsibility (Load()) to clamp N2 <= N1.
func (s SynthesisConfig) MaxRelatedSynthesesOr(def int) int {
	if s.MaxRelatedSyntheses > 0 {
		return s.MaxRelatedSyntheses
	}
	return def
}

// ReportsConfig configures the report autofact-annotation pipeline.
// The annotate_report worker chunks an uploaded report into sentences,
// embeds each with the same ai.EmbeddingProvider the facts use, and
// searches the okt_facts Qdrant collection for similar facts above the
// configured threshold; matches persist in report_annotations.
//
// Enabled is a soft gate: when false the worker logs and is a no-op
// (the create/upload endpoints still accept reports, they just stay in
// `pending` until the operator enables annotation). SimilarityThreshold
// is the cosine similarity score (0..1) above which a fact is considered
// a supporting citation — lower than dedup's 0.94 because we want
// "supporting", not "duplicate". MaxFactsPerSentence caps how many
// matches a single sentence keeps (the top-N by score). MinSentenceRunes
// skips headings/fragments too short to be worth embedding.
type ReportsConfig struct {
	Enabled             bool                  `mapstructure:"enabled"`
	SimilarityThreshold float64               `mapstructure:"similarity_threshold"`   // 0.84
	MaxFactsPerSentence int                   `mapstructure:"max_facts_per_sentence"` // 5
	MinSentenceRunes    int                   `mapstructure:"min_sentence_runes"`     // 40
	PostureClassifier   PostureClassifierConfig `mapstructure:"posture_classifier"`
}

// PostureClassifierConfig configures the autocite posture classifier,
// an LLM pass that runs between Qdrant retrieval and annotation
// persistence. For every (sentence, candidate fact) pair the
// classifier assigns one of four postures — related, supports,
// contradicts, irrelevant — and the worker drops irrelevant pairs
// before writing report_annotations. When the chat/AI provider is
// not configured (or Enabled is false, or the per-repo
// posture_classifier_enabled flag is off) the worker falls back to
// the legacy keep-all behavior and stores rows with posture = NULL.
//
// Provider+Model resolve the same way the other AI-using tasks do:
// the deployment's ai.Providers catalog is looked up by Provider
// and the model id is passed to ChatRequest.Model. A per-repo
// repository_model_settings row for task_kind='report_annotation'
// overrides both (via ModelResolver) without touching the global
// config. BatchSize is sentences per LLM call (multi-sentence
// batching so a report produces a few calls, not one per sentence);
// MaxConcurrent bounds in-flight calls per worker.
type PostureClassifierConfig struct {
	Enabled       bool   `mapstructure:"enabled"`         // global on/off; default true
	Provider      string `mapstructure:"provider"`        // ai provider id, e.g. "openrouter"
	Model         string `mapstructure:"model"`           // chat model id
	BatchSize     int    `mapstructure:"batch_size"`      // sentences per LLM call; default 8
	MaxConcurrent int    `mapstructure:"max_concurrent"`  // in-flight LLM calls per worker; default 4
	MaxTokens     int    `mapstructure:"max_tokens"`      // output token cap per batch; default 800
}

// SimilarityThresholdOr returns the configured threshold or the
// default (0.84) when zero/negative. The worker threads this into
// Qdrant's ScoreThreshold so only matches above it are returned.
// A per-repository override (repository_report_settings) takes
// precedence over this value when set.
func (r ReportsConfig) SimilarityThresholdOr(def float64) float64 {
	if r.SimilarityThreshold > 0 {
		return r.SimilarityThreshold
	}
	return def
}

// BatchSizeOr returns the configured posture-classifier BatchSize
// or the default (8) when zero/negative.
func (p PostureClassifierConfig) BatchSizeOr(def int) int {
	if p.BatchSize > 0 {
		return p.BatchSize
	}
	return def
}

// MaxConcurrentOr returns the configured posture-classifier
// MaxConcurrent or the default (4) when zero/negative.
func (p PostureClassifierConfig) MaxConcurrentOr(def int) int {
	if p.MaxConcurrent > 0 {
		return p.MaxConcurrent
	}
	return def
}

// MaxTokensOr returns the configured posture-classifier MaxTokens
// or the default (800) when zero/negative.
func (p PostureClassifierConfig) MaxTokensOr(def int) int {
	if p.MaxTokens > 0 {
		return p.MaxTokens
	}
	return def
}

// MaxFactsPerSentenceOr returns the configured cap or the default
// (5) when zero/negative. Bounds the annotation density so a long
// report doesn't produce an unbounded number of citations per
// sentence.
func (r ReportsConfig) MaxFactsPerSentenceOr(def int) int {
	if r.MaxFactsPerSentence > 0 {
		return r.MaxFactsPerSentence
	}
	return def
}

// MinSentenceRunesOr returns the configured minimum sentence
// length or the default (40) when zero/negative. Sentences shorter
// than this (headings, list markers, one-word lines) are skipped
// because they are too short to produce a meaningful embedding.
func (r ReportsConfig) MinSentenceRunesOr(def int) int {
	if r.MinSentenceRunes > 0 {
		return r.MinSentenceRunes
	}
	return def
}

type AIProvidersConfig struct {
	Ollama      OllamaProviderConfig      `mapstructure:"ollama"`
	OllamaCloud OllamaCloudProviderConfig `mapstructure:"ollama_cloud"`
	OpenRouter  OpenRouterProviderConfig  `mapstructure:"openrouter"`
	Models      []AIModelConfig           `mapstructure:"models"`
}

type OllamaProviderConfig struct {
	BaseURL string `mapstructure:"base_url"`
}

type OllamaCloudProviderConfig struct {
	APIKey string `mapstructure:"api_key"`
}

type OpenRouterProviderConfig struct {
	APIKey string `mapstructure:"api_key"`
	// EmbedBatchSize is the max number of inputs sent in a single
	// POST /v1/embeddings request. OpenRouter (and the OpenAI-compatible
	// endpoint it proxies) returns an empty `data` array when a batch
	// exceeds the underlying embedding model's input-count or total-token
	// limit. Lower this if embed_facts fails with
	// "embed response has no data". Defaults to 64 when unset or <=0.
	EmbedBatchSize int `mapstructure:"embed_batch_size"`
}

type AIModelConfig struct {
	ID             string  `mapstructure:"id"`
	Provider       string  `mapstructure:"provider"`
	InputCostPer1M float64 `mapstructure:"input_cost_per_1m"`
	OutputCostPer1M float64 `mapstructure:"output_cost_per_1m"`
	ThinkingLevel  *string `mapstructure:"thinking_level"`
	// RateLimitRPM caps the number of requests-per-minute sent to
	// this model. 0 means unlimited (no limiter installed for this
	// model). When unset, the wiring layer defaults to
	// DefaultModelRateLimitRPM (30) so every model gets a sane
	// ceiling out of the box — set to 0 explicitly to opt out. The
	// limit is enforced per-model (not per-provider) via a
	// rate.Limiter in the RateLimitedProvider decorator, so a
	// provider serving multiple models gets one bucket per model.
	RateLimitRPM int `mapstructure:"rate_limit_rpm"`
}

// DefaultModelRateLimitRPM is the per-model requests-per-minute
// ceiling applied when a model entry omits rate_limit_rpm. 30 RPM
// is a conservative default that keeps a single model from
// drowning an LLM provider when the queue fans out hundreds of
// workers; operators raising it (or setting 0 for unlimited) is
// the intended tuning knob. See ratelimit.go for the decorator.
const DefaultModelRateLimitRPM = 30

type SearchProvidersConfig struct {
	Provider string                 `mapstructure:"provider"`
	Serper   SerperProviderConfig   `mapstructure:"serper"`
	OpenAlex OpenAlexProviderConfig `mapstructure:"openalex"`
}

type SerperProviderConfig struct {
	APIKey string `mapstructure:"api_key"`
}

type OpenAlexProviderConfig struct {
	Email string `mapstructure:"email"`
}

type ResolutionProvidersConfig struct {
	Fetch        FetchResolutionConfig   `mapstructure:"fetch"`
	Unpaywall    UnpaywallProviderConfig `mapstructure:"unpaywall"`
	TLS          TLSImpersonationConfig  `mapstructure:"tls"`
	FlareSolverr FlareSolverrConfig      `mapstructure:"flaresolverr"`
	// HostOverrides is a static host → provider-id map the
	// strategy consults before learning. An operator can pin
	// "www.cell.com" → "tls" so the TLS-impersonation tier
	// runs first for that host without waiting for the
	// learned preference to converge. Empty means no static
	// overrides (the chain order is the default).
	HostOverrides map[string]string `mapstructure:"host_overrides"`
	// Chain is the ordered, comma-separated list of provider
	// ids the strategy should try. When empty the strategy
	// uses the order providers were registered in. This is
	// an operator escape hatch for reordering without code
	// changes (e.g. "unpaywall,tls,fetch,flaresolverr").
	Chain string `mapstructure:"chain"`
}

type FetchResolutionConfig struct {
	Enabled   bool              `mapstructure:"enabled"`
	UserAgent string            `mapstructure:"user_agent"`
	Timeout   time.Duration     `mapstructure:"timeout"`
	Retry     FetchRetryConfig  `mapstructure:"retry"`
}

// FetchRetryConfig configures retry behaviour for the HTTP fetch
// and TLS impersonation resolution providers. A MaxAttempts of 1
// disables retry. BaseDelay and MaxDelay use exponential backoff.
type FetchRetryConfig struct {
	MaxAttempts int           `mapstructure:"max_attempts"`
	BaseDelay   time.Duration `mapstructure:"base_delay"`
	MaxDelay    time.Duration `mapstructure:"max_delay"`
}

// UnpaywallProviderConfig configures the Unpaywall v2
// resolution provider. Email is the contact string
// Unpaywall requires as a `?email=` query parameter on
// every API request; it also acts as the de-facto API
// identification. When empty, the provider is disabled
// (the constructor returns nil and the wiring layer
// simply doesn't register it). The field can also be
// supplied via the UNPAYWALL_EMAIL env var so docker
// compose / secrets managers can keep the address out of
// the YAML.
type UnpaywallProviderConfig struct {
	Email string `mapstructure:"email"`
}

// TLSImpersonationConfig configures the TLS-impersonation
// resolution provider (Phase 3). When Impersonate is empty
// the provider self-disables and is not registered in the
// chain. Impersonate is the browser fingerprint target
// (e.g. "chrome_124"); the underlying tls-client library
// accepts the same identifiers as curl-impersonate.
type TLSImpersonationConfig struct {
	Impersonate string        `mapstructure:"impersonate"`
	Timeout     time.Duration `mapstructure:"timeout"`
}

// FlareSolverrConfig configures the headless-browser
// resolution provider (Phase 3). When URL is empty AND
// Endpoints is empty the provider self-disables and is not
// registered. URL is the FlareSolverr / Byparr HTTP endpoint
// (e.g. "http://flaresolverr:8191") for a single-instance
// deployment. Endpoints is a list of endpoints for a
// horizontally-scaled deployment (one entry per Byparr
// container); the provider round-robins across them. When
// both are set, Endpoints takes precedence and URL is
// appended to the pool. Timeout is the per-request budget
// for the headless browser; defaults to 60s when empty,
// parsed as a Go time.Duration string. MaxConcurrency caps
// the number of in-flight Resolve calls across the whole
// pool — a single Byparr container drives one headless
// Chromium and queues concurrent requests internally, so
// allowing more in-flight calls than containers just burns
// the timeout budget waiting in the sidecar's queue. The
// default of 0 means "no application-level cap" (the
// sidecar's own HTTP server is the only limit); set it to
// roughly the number of Byparr containers × the per-container
// concurrency you want to allow (typically 1×containers for
// challenge-heavy workloads, 2×containers for lighter pages).
type FlareSolverrConfig struct {
	URL            string        `mapstructure:"url"`
	Endpoints      []string      `mapstructure:"endpoints"`
	Timeout        time.Duration `mapstructure:"timeout"`
	MaxConcurrency int           `mapstructure:"max_concurrency"`
}

type ServerConfig struct {
	Port         int           `mapstructure:"port"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout"`
}

type DatabaseConfig struct {
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	User     string `mapstructure:"user"`
	Password string `mapstructure:"password"`
	Name     string `mapstructure:"name"`
	SSLMode  string `mapstructure:"ssl_mode"`
	MaxConns int    `mapstructure:"max_conns"`
}

func (d DatabaseConfig) DSN() string {
	return fmt.Sprintf(
		"postgres://%s:%s@%s:%d/%s?sslmode=%s",
		d.User, d.Password, d.Host, d.Port, d.Name, d.SSLMode,
	)
}

// TaskConfig configures the background task manager. In local
// development it defaults to the same database as the app, but in
// production it may be split out into its own database (e.g. a
// dedicated "okt_tasks" instance).
//
// The preferred form is `task.database: <name>` (referencing a
// `databases.<name>` block). The legacy fields below are still
// parsed for backward compatibility; when any of them is set,
// `Load()` synthesizes a `databases.tasks` block from them and
// sets `task.database: tasks`.
type TaskConfig struct {
	// Database is the registered database the task manager
	// connects to. Empty falls back to "default". Must reference
	// a key in `Databases`.
	Database string `mapstructure:"database"`
	// Host/Port/User/Password/Name/SSLMode are legacy fields. In
	// the new world they are parsed but only used to synthesize
	// `databases.tasks` for backward compatibility.
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	User     string `mapstructure:"user"`
	Password string `mapstructure:"password"`
	Name     string `mapstructure:"name"`
	SSLMode  string `mapstructure:"ssl_mode"`
	// Queues configures the worker queues the River client should
	// run. Map keys are queue names; values are their max worker
	// counts. When empty, a single "default" queue with 100 workers
	// is used.
	Queues map[string]int `mapstructure:"queues"`
	// JobTimeout is the maximum wall-clock time a single River job
	// is allowed to run before River cancels its context. Defaults
	// to 4h via config.default.yaml so that long sources (full
	// books with hundreds of chunks + image extraction) complete
	// in one job; small jobs finish early so the long budget costs
	// nothing. Overrides via `task.job_timeout` (any Go duration
	// string accepted by time.ParseDuration). Set to 0 to disable
	// River's job-level timeout entirely (jobs then run until the
	// worker process exits or the ctx is cancelled).
	JobTimeout time.Duration `mapstructure:"job_timeout"`
	// HeartbeatInterval is how often the worker process updates
	// its last_heartbeat row in okt_worker_heartbeat. Default 1m.
	// The heartbeat is sent by a background goroutine independent of
	// job execution, so even a 4h retrieve_source job keeps the
	// worker marked alive.
	HeartbeatInterval time.Duration `mapstructure:"heartbeat_interval"`
	// HeartbeatTimeout is the staleness threshold: a worker whose
	// last_heartbeat is older than this is considered dead. Default
	// 10m. The rescue query resets running jobs whose current owner
	// (attempted_by[last]) has a stale or missing heartbeat back to
	// available. Must be > HeartbeatInterval.
	HeartbeatTimeout time.Duration `mapstructure:"heartbeat_timeout"`
	// RescueOnStartup enables the rescue query that runs once in
	// Manager.Start() right after the River client starts. Default
	// true. Set to false to disable (e.g. for debugging). The
	// on-demand POST /admin/tasks/rescue endpoint is unaffected.
	RescueOnStartup bool `mapstructure:"rescue_on_startup"`
	// RefreshConceptRelationsInterval is the cadence of the periodic
	// concept-relations matview refresh. Every interval, a periodic
	// job fans out one RefreshConceptRelationsArgs per registered
	// database; the per-database worker dedups concurrent enqueues so
	// only one refresh runs per database at a time. Defaults to 10m
	// when zero/unset. The matview is ALSO refreshed at the end of
	// every extract_concepts batch, so this periodic is a safety net
	// for repos with no recent extraction (and for catching up after
	// a deploy that adds the matview to an existing database).
	RefreshConceptRelationsInterval time.Duration `mapstructure:"refresh_concept_relations_interval"`
}

// DSN builds the PostgreSQL connection string for the task database,
// falling back to the provided app database values for any field that
// is left empty in the task config. This is what makes "use the same
// database as the app in local" work without duplicating config.
//
// Deprecated: keep around for one release for the legacy code path.
// New code should resolve a *pgxpool.Pool from the dbpool.Registry
// (using cfg.Task.Database) and not hand-roll DSNs.
func (t TaskConfig) DSN(app DatabaseConfig) string {
	host := t.Host
	if host == "" {
		host = app.Host
	}
	port := t.Port
	if port == 0 {
		port = app.Port
	}
	user := t.User
	if user == "" {
		user = app.User
	}
	password := t.Password
	if password == "" {
		password = app.Password
	}
	name := t.Name
	if name == "" {
		name = app.Name
	}
	sslMode := t.SSLMode
	if sslMode == "" {
		sslMode = app.SSLMode
	}
	return fmt.Sprintf(
		"postgres://%s:%s@%s:%d/%s?sslmode=%s",
		user, password, host, port, name, sslMode,
	)
}

// hasLegacyTaskFields reports whether any of the legacy
// task.{host,port,user,password,name,sslmode} fields are set, so
// `Load()` can decide whether to synthesize a `databases.tasks`
// block from them.
func (t TaskConfig) hasLegacyTaskFields() bool {
	return t.Host != "" || t.Port != 0 || t.User != "" || t.Password != "" || t.Name != "" || t.SSLMode != ""
}

type AuthConfig struct {
	JWTSecret string        `mapstructure:"jwt_secret"`
	TokenTTL  time.Duration `mapstructure:"token_ttl"`
}

// Load reads the configuration from the layered set of sources
// described below and returns the parsed *Config.
//
// configPath is the optional `--config` flag value. It may be:
//
//   - empty: the loader searches the standard on-disk paths (in
//     order): `--config` is not set, so it is skipped; then
//     `./configs`, `.`, `<binary_dir>/configs`, `<binary_dir>`.
//   - a path to a file: that file is loaded directly as the default
//     config; a sibling `config.local.yaml` (if present) is merged
//     on top.
//   - a path to a directory: that directory is searched first for
//     `config.default.yaml` / `config.local.yaml`, ahead of the
//     standard paths.
//   - a path that does not exist: a warning is logged and the
//     loader falls through to the standard search; the app still
//     boots (via the embedded fallback if needed).
//
// If no on-disk `config.default.yaml` is found in any search path,
// the loader falls back to the embedded copy bundled into the
// binary (configs.DefaultConfigFS, see backend/configs/embed.go).
// In that case, when the binary's directory is writable, the
// embedded default is also written to
// `<binary_dir>/configs/config.default.yaml` so an operator gets
// an editable file on first run without having to copy one out.
// The write is best-effort: a non-writable binary dir only disables
// the auto-write, not the boot.
//
// `.env` files are searched in the CWD, the binary's directory,
// and (when --config is a directory) the config directory; the
// first existing one wins. Env vars always override YAML.
func Load(configPath string) (*Config, error) {
	binDir := resolveBinaryDir()

	// `.env` discovery. Try CWD first (preserves the dev workflow),
	// then the binary's directory (for shipped-alongside-binary
	// deploys), then (when --config is a dir) the config dir.
	loadDotEnv(".", binDir, configPath)

	v := viper.New()
	v.SetConfigType("yaml")

	_, usedEmbeddedDefault, err := loadDefaultConfig(v, configPath, binDir)
	if err != nil {
		return nil, err
	}

	// Merge config.local.yaml on top, if present. The search
	// reuses the AddConfigPath list already registered for the
	// default; a config.local.yaml in any of those dirs wins
	// (earliest path first). When the default came from the
	// embedded fallback there is no local config to merge (the
	// operator can add one by creating config.local.yaml in the
	// binary's configs/ dir — the auto-write step below creates
	// that dir, so subsequent boots will find it).
	if !usedEmbeddedDefault {
		v.SetConfigName("config.local")
		if err := v.MergeInConfig(); err != nil {
			if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
				return nil, fmt.Errorf("reading local config: %w", err)
			}
		}
	}

	// First-run auto-write: when the default came from the
	// embedded fallback, write it next to the binary so the
	// operator gets an editable file. Best-effort; a non-writable
	// binary dir is logged and ignored.
	if usedEmbeddedDefault && binDir != "" {
		writeEmbeddedDefaultToDisk(binDir)
	}

	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	for _, key := range v.AllKeys() {
		v.Set(key, v.Get(key))
	}

	var cfg Config
	// UnmarshalExact sets mapstructure's ErrorUnused=true so a key
	// the Config struct doesn't know about (e.g. a `task:` block
	// accidentally indented under `isolation:`, or a typo like
	// `provders:`) fails loudly at boot instead of being silently
	// dropped. The previous `v.Unmarshal` silently ignored unknown
	// keys, which is how the misplaced `task:` block in
	// config.default.yaml shipped without anyone noticing: River
	// then booted with only the catch-all `default` queue, the
	// per-task queues were never declared, and every enqueued job
	// sat in `available` forever. Failing at boot is the correct
	// behavior — a misconfigured app is non-functional anyway.
	if err := v.UnmarshalExact(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshaling config: %w", err)
	}

	// Synthesize `databases` from the legacy `database:` block and
	// from the legacy `task.{host,...}` fields, when the operator
	// hasn't declared them in the new shape. The synthesis runs
	// before the env-var alias step because we want the YAML-
	// supplied values to be the "ground truth" that the env vars
	// may override; if the alias ran first it would pre-create
	// `databases.default` and the synthesis would short-circuit.
	if cfg.Databases == nil {
		cfg.Databases = make(map[string]DatabaseConfig)
	}
	if _, ok := cfg.Databases["default"]; !ok {
		if (cfg.Database == DatabaseConfig{}) {
			return nil, fmt.Errorf("config: no databases declared and no legacy `database:` block to fall back on; set `databases.default` (or the legacy `database:` block) in your config")
		}
		cfg.Databases["default"] = cfg.Database
		log.Println("config: legacy `database:` block detected — please migrate to `databases.default` in a future release")
	}

	// Legacy env-var aliases. The original config shape
	// exposed a single `database:` block; the operator's
	// docker-compose and other deploys set `DATABASE_HOST`
	// etc. to override it. The new `databases:` map shape
	// means viper's AutomaticEnv would look for
	// `DATABASES_DEFAULT_HOST` instead. We bridge the gap by
	// reading the legacy env vars directly and writing them
	// into the new keys before any other code sees the
	// config. This is intentionally the same shape the
	// pre-refactor code used, just routed through viper.
	applyLegacyDatabaseEnvAliases(&cfg)

	// REGISTRY_URL env-var alias. Viper's AutomaticEnv maps
	// PROVIDERS_REGISTRY_URL to providers.registry.url, but
	// docker-compose and deploys set the shorter REGISTRY_URL.
	// Read it directly and write the config value, same pattern
	// as the legacy DATABASE_HOST / TASK_HOST aliases above.
	if v := os.Getenv("REGISTRY_URL"); v != "" {
		cfg.Providers.Registry.URL = v
	}
	if v := os.Getenv("REGISTRY_AUTH_MODE"); v != "" {
		cfg.Providers.Registry.AuthMode = v
	}
	if v := os.Getenv("REGISTRY_API_KEY"); v != "" {
		cfg.Providers.Registry.APIKey = v
	}
	if v := os.Getenv("REGISTRY_READ_API_KEY"); v != "" {
		cfg.Providers.Registry.ReadAPIKey = v
	}

	// Normalize the registries list: when the operator only set the
	// legacy single `providers.registry` block (or the REGISTRY_*
	// env vars), ResolveRegistries synthesizes a one-element list
	// with id "default". When `registries[]` is set, ensure every
	// entry has a stable id (ResolveRegistries fills empty ones).
	// We don't mutate cfg here — callers always go through
	// ProvidersConfig.ResolveRegistries / RegistryByID.

	if cfg.Task.hasLegacyTaskFields() {
		if _, ok := cfg.Databases["tasks"]; !ok {
			t := cfg.Task
			legacy := DatabaseConfig{
				Host:     t.Host,
				Port:     t.Port,
				User:     t.User,
				Password: t.Password,
				Name:     t.Name,
				SSLMode:  t.SSLMode,
				MaxConns: 10,
			}
			if legacy.Host == "" {
				legacy.Host = cfg.Databases["default"].Host
			}
			if legacy.Port == 0 {
				legacy.Port = cfg.Databases["default"].Port
			}
			if legacy.User == "" {
				legacy.User = cfg.Databases["default"].User
			}
			if legacy.Password == "" {
				legacy.Password = cfg.Databases["default"].Password
			}
			if legacy.Name == "" {
				legacy.Name = cfg.Databases["default"].Name
			}
			if legacy.SSLMode == "" {
				legacy.SSLMode = cfg.Databases["default"].SSLMode
			}
			cfg.Databases["tasks"] = legacy
			if cfg.Task.Database == "" {
				cfg.Task.Database = "tasks"
			}
			log.Println("config: legacy `task.{host,port,...}` fields detected — please migrate to `databases.tasks` in a future release")
		}
	}
	if cfg.Task.Database == "" {
		cfg.Task.Database = "default"
	}
	if cfg.System.Database == "" {
		cfg.System.Database = "default"
	}
	if cfg.Isolation.DefaultDatabase == "" {
		cfg.Isolation.DefaultDatabase = "default"
	}
	// Storage defaults: when the operator didn't configure a
	// backend at all, default to the local filesystem under
	// `var/source_assets` so the app works out of the box. The
	// default is safe for local dev; production deploys should
	// mount a persistent volume at that path or switch to the
	// future S3 backend.
	if cfg.Providers.Storage.Backend == "" {
		cfg.Providers.Storage.Backend = "filesystem"
	}
	if cfg.Providers.Storage.Backend == "filesystem" && cfg.Providers.Storage.Filesystem.Root == "" {
		cfg.Providers.Storage.Filesystem.Root = "var/source_assets"
	}
	// AllowedDatabases is left empty by default (the picker is
	// closed). Operators explicitly opt in to multi-DB by listing
	// databases. The validation step below also ensures every
	// entry exists in cfg.Databases.

	// Synthesis related-concept clamp: N2 (syntheses to attach)
	// must be <= N1 (related concept names loaded). Apply defaults
	// via the *Or helpers so an operator who sets only one of the
	// two still gets a sane pair, then clamp N2 down to N1 if the
	// operator set N2 > N1 explicitly.
	n1 := cfg.Providers.Synthesis.MaxRelatedConceptsOr(10)
	n2 := cfg.Providers.Synthesis.MaxRelatedSynthesesOr(3)
	if n2 > n1 {
		n2 = n1
	}
	cfg.Providers.Synthesis.MaxRelatedConcepts = n1
	cfg.Providers.Synthesis.MaxRelatedSyntheses = n2

	// Validate cross-references. Bail at boot if the operator
	// referenced a database that doesn't exist; we want this to be
	// loud, not a 500 on the first request.
	if err := validate(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// resolveBinaryDir returns the directory containing the running
// binary, or "" if it cannot be determined (e.g. `go run`, which
// puts the binary in a temp dir that may have been cleaned up).
// Used as a search path and as the auto-write target for the
// embedded default config.
func resolveBinaryDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		// EvalSymlinks can fail on some platforms when the path
		// is already canonical; fall back to the raw path.
		exe, _ = filepath.Abs(exe)
	}
	return filepath.Dir(exe)
}

// loadDotEnv loads .env from the first location that has one. The
// candidate dirs are: "." (CWD, the dev workflow default), binDir
// (shipped-alongside-binary), and (when configPath is a dir) the
// config dir. godotenv.Load errors when the file is missing, which
// is the common case, so we swallow the not-found error and only
// propagate parse errors from an existing file.
func loadDotEnv(dirs ...string) {
	for _, d := range dirs {
		if d == "" {
			continue
		}
		p := filepath.Join(d, ".env")
		if _, err := os.Stat(p); err != nil {
			continue
		}
		// godotenv.Load merges into os.Environ without overwriting
		// already-set vars; calling it repeatedly is safe.
		if err := godotenv.Load(p); err != nil {
			// A parse error in an existing .env is worth surfacing
			// but not fatal — log and continue so a broken .env
			// doesn't block boot when the YAML is fine.
			log.Printf("config: ignoring %s: %v", p, err)
		}
	}
}

// loadDefaultConfig finds and reads config.default.yaml. It
// returns:
//   - defaultDir: the directory the default was loaded from, or ""
//     when it came from the embedded fallback.
//   - usedEmbeddedDefault: true when the on-disk search failed and
//     the embedded copy was used instead.
//   - err: any read/parse error from the default config.
//
// Search order when configPath is empty:
//  1. ./configs
//  2. .
//  3. <binDir>/configs
//  4. <binDir>
//  5. embedded fallback
//
// When configPath is a file, that file is used directly. When
// configPath is a directory, it is searched first (ahead of the
// standard paths). When configPath does not exist, a warning is
// logged and the standard search proceeds.
func loadDefaultConfig(v *viper.Viper, configPath, binDir string) (defaultDir string, usedEmbedded bool, err error) {
	// Explicit --config flag: file or directory, auto-detected.
	if configPath != "" {
		info, statErr := os.Stat(configPath)
		if statErr != nil {
			log.Printf("config: --config %q not found; falling back to standard search: %v", configPath, statErr)
		} else if info.IsDir() {
			v.SetConfigName("config.default")
			v.AddConfigPath(configPath)
			addStandardSearchPaths(v, binDir)
			if rerr := v.ReadInConfig(); rerr != nil {
				return "", false, fmt.Errorf("reading default config: %w", rerr)
			}
			return filepath.Dir(v.ConfigFileUsed()), false, nil
		} else {
			v.SetConfigFile(configPath)
			if rerr := v.ReadInConfig(); rerr != nil {
				return "", false, fmt.Errorf("reading default config: %w", rerr)
			}
			return filepath.Dir(configPath), false, nil
		}
	}

	// Standard on-disk search.
	v.SetConfigName("config.default")
	addStandardSearchPaths(v, binDir)
	if rerr := v.ReadInConfig(); rerr != nil {
		// ConfigFileNotFoundError is the signal to fall back to
		// the embedded default; anything else is a real parse
		// error we should surface.
		if _, ok := rerr.(viper.ConfigFileNotFoundError); !ok {
			return "", false, fmt.Errorf("reading default config: %w", rerr)
		}
		// Fall through to the embedded default.
	} else {
		return filepath.Dir(v.ConfigFileUsed()), false, nil
	}

	// Embedded fallback: load the bundled config.default.yaml
	// straight from the binary.
	embedded, rerr := configs.DefaultConfigBytes()
	if rerr != nil {
		return "", false, fmt.Errorf("reading embedded default config: %w", rerr)
	}
	// viper.ReadConfig reads from the io.Reader into the current
	// config state. We reset the config name/type so the later
	// MergeInConfig call for config.local.yaml doesn't try to
	// re-read the in-memory buffer.
	v.SetConfigName("config.default")
	v.SetConfigType("yaml")
	if rerr := v.ReadConfig(bytes.NewReader(embedded)); rerr != nil {
		return "", false, fmt.Errorf("parsing embedded default config: %w", rerr)
	}
	log.Println("config: no on-disk config.default.yaml found; using the embedded default")
	return "", true, nil
}

// addStandardSearchPaths registers the on-disk search paths used
// by both the empty-configPath path and the directory-configPath
// path. Order matters: earlier paths win. The dev workflow
// (./configs, .) is preserved first; the binary-relative paths
// (for shipped-alongside-binary deploys) come last so a developer
// with a configs/ dir in CWD isn't accidentally overridden by a
// stale copy next to the binary.
func addStandardSearchPaths(v *viper.Viper, binDir string) {
	v.AddConfigPath("./configs")
	v.AddConfigPath(".")
	if binDir != "" {
		v.AddConfigPath(filepath.Join(binDir, "configs"))
		v.AddConfigPath(binDir)
	}
}

// writeEmbeddedDefaultToDisk writes the embedded config.default.yaml
// to <binDir>/configs/config.default.yaml so an operator gets an
// editable file on first run. Best-effort: a non-writable binDir
// only disables the auto-write, not the boot. Never overwrites an
// existing file (respects operator edits).
func writeEmbeddedDefaultToDisk(binDir string) {
	targetDir := filepath.Join(binDir, "configs")
	target := filepath.Join(targetDir, "config.default.yaml")
	// Don't clobber an existing file — respect the operator's
	// edits even when the on-disk search somehow missed it (e.g.
	// race with a concurrent writer).
	if _, err := os.Stat(target); err == nil {
		return
	} else if !os.IsNotExist(err) {
		// A stat error other than NotExist is unusual; log and
		// skip rather than guess.
		log.Printf("config: skipping auto-write of default config to %s: stat: %v", target, err)
		return
	}
	embedded, err := configs.DefaultConfigBytes()
	if err != nil {
		log.Printf("config: skipping auto-write of default config: %v", err)
		return
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		log.Printf("config: could not create %s for auto-write of default config: %v (booting with the embedded copy)", targetDir, err)
		return
	}
	if err := os.WriteFile(target, embedded, 0o644); err != nil {
		log.Printf("config: could not auto-write default config to %s: %v (booting with the embedded copy)", target, err)
		return
	}
	log.Printf("config: wrote default config to %s (edit it to customize; config.local.yaml in the same dir overrides it)", target)
}

func validate(cfg *Config) error {
	if _, ok := cfg.Databases["default"]; !ok {
		return fmt.Errorf("config: `databases.default` is required")
	}
	mustExist := map[string]string{
		"task.database":              cfg.Task.Database,
		"system.database":            cfg.System.Database,
		"isolation.default_database": cfg.Isolation.DefaultDatabase,
	}
	for label, name := range mustExist {
		if _, ok := cfg.Databases[name]; !ok {
			return fmt.Errorf("config: %s references %q which is not in `databases`", label, name)
		}
	}
	// Every database listed under isolation.allowed_databases
	// must exist in cfg.Databases. Operators that want the
	// picker closed leave the slice empty.
	for _, name := range cfg.Isolation.AllowedDatabases {
		if _, ok := cfg.Databases[name]; !ok {
			return fmt.Errorf("config: `isolation.allowed_databases` contains %q which is not in `databases`", name)
		}
	}
	// Storage backend must be one of the known implementations.
	// The default is "filesystem"; an operator who sets an
	// unknown backend gets a loud boot error instead of silent
	// data loss.
	switch cfg.Providers.Storage.Backend {
	case "filesystem":
		if cfg.Providers.Storage.Filesystem.Root == "" {
			return fmt.Errorf("config: `providers.storage.filesystem.root` is required when backend is filesystem")
		}
	case "s3":
		return fmt.Errorf("config: `providers.storage.backend` = %q is reserved for the future cloud provider and not yet implemented; use `filesystem`", cfg.Providers.Storage.Backend)
	default:
		return fmt.Errorf("config: `providers.storage.backend` = %q is not supported (known: filesystem, s3)", cfg.Providers.Storage.Backend)
	}
	return nil
}

// applyLegacyDatabaseEnvAliases reads the legacy `DATABASE_*` and
// `TASK_*` env vars and writes them into the new config shape
// (`databases.default.*`, `task.database` → `databases.tasks.*`).
// The pre-refactor code did this implicitly through viper; the new
// map-based shape needs an explicit step because viper's
// `AutomaticEnv` only matches keys that are already in the config
// tree, and the new keys (`databases.default.host`) don't match
// the old env-var names (`DATABASE_HOST`).
//
// Precedence: env > YAML > default. Setting `DATABASE_HOST=postgres`
// in the deploy environment overrides any `databases.default.host`
// the operator put in the YAML, which is the behavior every
// docker-compose deploy (and the original pre-refactor code)
// relied on. We do this here because the legacy env-var names
// are the public contract for deploys; once we drop the alias
// step, an operator migrating from the old shape to the new
// shape would have to rename `DATABASE_HOST` to
// `DATABASES_DEFAULT_HOST` and find out the hard way.
func applyLegacyDatabaseEnvAliases(cfg *Config) {
	if cfg.Databases == nil {
		cfg.Databases = make(map[string]DatabaseConfig)
	}
	def := cfg.Databases["default"]
	if v := os.Getenv("DATABASE_HOST"); v != "" {
		def.Host = v
	}
	if v := os.Getenv("DATABASE_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			def.Port = n
		}
	}
	if v := os.Getenv("DATABASE_USER"); v != "" {
		def.User = v
	}
	if v := os.Getenv("DATABASE_PASSWORD"); v != "" {
		def.Password = v
	}
	if v := os.Getenv("DATABASE_NAME"); v != "" {
		def.Name = v
	}
	if v := os.Getenv("DATABASE_SSLMODE"); v != "" {
		def.SSLMode = v
	}
	cfg.Databases["default"] = def

	// Mirror the legacy task.* fields into a `databases.tasks`
	// entry when the operator has set TASK_* env vars. We only
	// allocate the entry if at least one of the legacy env vars
	// is set; otherwise we leave `databases` alone and let the
	// later synthesis step (which reads cfg.Task fields) decide
	// whether to create it.
	if !anyTaskEnv() {
		return
	}
	if _, ok := cfg.Databases["tasks"]; !ok {
		cfg.Databases["tasks"] = cfg.Databases["default"]
	}
	t := cfg.Databases["tasks"]
	if v := os.Getenv("TASK_HOST"); v != "" {
		t.Host = v
	}
	if v := os.Getenv("TASK_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			t.Port = n
		}
	}
	if v := os.Getenv("TASK_USER"); v != "" {
		t.User = v
	}
	if v := os.Getenv("TASK_PASSWORD"); v != "" {
		t.Password = v
	}
	if v := os.Getenv("TASK_NAME"); v != "" {
		t.Name = v
		// Pointing task at a separate database is a deliberate
		// choice. Setting any TASK_* env var is the operator's
		// signal that they want a dedicated task DB; force
		// `task.database` to the synthesized `tasks` entry so
		// the registry and taskmanager pick up the dedicated
		// pool instead of falling back to the YAML default.
		// Without this override the YAML value
		// (`task.database: default`) would silently win and the
		// application would keep using the shared pool — the
		// exact failure mode the docker-compose wiring is
		// designed to prevent.
		cfg.Task.Database = "tasks"
	}
	if v := os.Getenv("TASK_SSLMODE"); v != "" {
		t.SSLMode = v
	}
	cfg.Databases["tasks"] = t
}

// anyTaskEnv reports whether any of the legacy TASK_* env vars
// is set. Used to gate the env-var alias step so we don't
// allocate a `databases.tasks` entry that the operator never
// asked for.
func anyTaskEnv() bool {
	for _, k := range []string{"TASK_HOST", "TASK_PORT", "TASK_USER", "TASK_PASSWORD", "TASK_NAME", "TASK_SSLMODE"} {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}
