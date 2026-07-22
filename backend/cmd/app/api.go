package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/internal/api"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/handler"
	"github.com/openktree/open-knowledge-tree/backend/internal/audit"
	"github.com/openktree/open-knowledge-tree/backend/internal/bootstrap"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/oauth"
	"github.com/openktree/open-knowledge-tree/backend/internal/promptset"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/content_parsing"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/decomposition"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/fetch"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ontology"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/refinement"
	registryclient "github.com/openktree/open-knowledge-tree/backend/internal/providers/registry"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/search"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/storage"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/summarization"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/synthesis"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/claims"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/posture"
	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/rbac"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/openktree/open-knowledge-tree/backend/internal/taskmanager"
	"github.com/openktree/open-knowledge-tree/backend/internal/taskmanager/tasks"
)

func runAPI(ctx context.Context, cfg *config.Config, queries *store.Queries, registry *dbpool.Registry) {
	// Build the registry client map + service map up front so the
	// registry search provider (the keyless default) can join the
	// searchProviders map alongside Serper/OpenAlex. The ClientMap
	// is also rebuilt later for the remote handler / pull workers —
	// both share the same config, so the two instances are
	// equivalent. Building it here lets a deployment with no
	// SERPER_API_KEY and no OPENALEX_EMAIL still get a working
	// search provider backed by the OKT Knowledge Registry.
	registryClients := registryclient.NewClientMap(cfg.Providers)
	registryServices := registryclient.NewServiceMap(registryClients)

	searchProviders := make(map[string]search.SearchProvider)

	serperKey := cfg.Providers.Search.Serper.APIKey
	if serperKey == "" {
		serperKey = os.Getenv("SERPER_API_KEY")
	}
	if serperKey != "" {
		searchProviders["serper"] = search.NewSerperSearchProvider(serperKey)
	}

	openAlexEmail := cfg.Providers.Search.OpenAlex.Email
	if openAlexEmail == "" {
		openAlexEmail = os.Getenv("OPENALEX_EMAIL")
	}
	if openAlexEmail != "" {
		searchProviders["openalex"] = search.NewOpenAlexSearchProvider(openAlexEmail)
	}

	// Register the registry search provider whenever any registry
	// is configured. It is the keyless default — a deployment with
	// no Serper/OpenAlex keys still gets a working search provider
	// so agents can discover sources other OKT instances have
	// contributed. When no registry is configured the ServiceMap is
	// a no-op and the provider self-reports "not configured" on
	// Search, so registering it is harmless.
	if registryServices.IsConfigured() {
		searchProviders["registry"] = search.NewRegistrySearchProvider(registryServices, cfg.Providers.Search.Registry)
	}

	// Default search provider precedence: the configured default
	// (cfg.Providers.Search.Provider, typically "serper") wins when
	// it's actually registered; otherwise fall back to "registry"
	// when the registry search provider is available, so a keyless
	// deployment still has a working default. The MCP handler and
	// the REST TestSearch handler both read this default when the
	// caller omits `provider`.
	defaultSearchProvider := cfg.Providers.Search.Provider
	if _, ok := searchProviders[defaultSearchProvider]; !ok {
		if _, ok := searchProviders["registry"]; ok {
			defaultSearchProvider = "registry"
		} else if len(searchProviders) > 0 {
			// Pick the first registered id (sorted for determinism)
			// so the default is never an unregistered id.
			ids := make([]string, 0, len(searchProviders))
			for k := range searchProviders {
				ids = append(ids, k)
			}
			sort.Strings(ids)
			defaultSearchProvider = ids[0]
		}
	}

	// Build the parser set shared by the plain fetch
	// provider and the Unpaywall OA-location fetch. The
	// previous wiring only registered Trafilatura, which
	// meant PDF responses (from OA pdf_url links or from
	// publisher PDFs) were detected as SourcePDF but no
	// parser claimed them, leaving Parsed empty and the
	// source row with parse_status='unsupported'. Adding
	// the MuPDF-backed FitzPDFParser makes PDFs fully
	// extractable in the same pass as HTML.
	parsers := []content_parsing.Parser{
		content_parsing.NewTrafilaturaParser(),
		content_parsing.NewFitzPDFParser(),
	}
	fetchTimeout := cfg.Providers.Resolution.Fetch.Timeout
	if fetchTimeout <= 0 {
		fetchTimeout = 60 * time.Second
	}
	fetchRetryCfg := fetch.RetryConfig{
		MaxAttempts: cfg.Providers.Resolution.Fetch.Retry.MaxAttempts,
		BaseDelay:   cfg.Providers.Resolution.Fetch.Retry.BaseDelay,
		MaxDelay:    cfg.Providers.Resolution.Fetch.Retry.MaxDelay,
	}
	// The config package uses the same zero-value-normalisation
	// convention: when all three retry fields are zero the config
	// was not set, so we pass a zero RetryConfig and let the
	// constructor apply defaultRetryConfig.
	fetchResolution := fetch.NewFetchResolutionProviderWithFullConfig(
		fetchTimeout, fetchRetryCfg, cfg.Providers.Resolution.Fetch.UserAgent, parsers...,
	)

	// Build the resolution-provider list in priority order:
	// Unpaywall is registered before the plain fetch so
	// DOI-classified sources hit Unpaywall's OA lookup
	// first. When the work has no OA copy Unpaywall
	// returns ErrUnpaywallNotOpenAccess, which the
	// strategy treats as a fall-through to the next
	// provider — the publisher landing page is then
	// fetched by fetchResolution, the same way it would
	// be today.
	//
	// Unpaywall now receives the same parser set so the
	// OA-location body (HTML or PDF) is parsed the same
	// way the plain fetch parses it. This closes the
	// previous gap where OA HTML fetched via Unpaywall
	// was never run through Trafilatura.
	//
	// NewUnpaywallResolutionProvider returns nil when
	// no email is configured, so the conditional is a
	// nil-safe filter rather than a guard.
	var resolutionProviders []fetch.ResolutionProvider
	unpaywallEmail := cfg.Providers.Resolution.Unpaywall.Email
	if unpaywallEmail == "" {
		unpaywallEmail = os.Getenv("UNPAYWALL_EMAIL")
	}
	if unpaywall := fetch.NewUnpaywallResolutionProviderWithParsers(unpaywallEmail, parsers...); unpaywall != nil {
		resolutionProviders = append(resolutionProviders, unpaywall)
	}

	// TLS-impersonation tier (config-gated, self-disables
	// when OKT_FETCH_IMPERSONATE / tls.impersonate is empty).
	// Runs after Unpaywall so OA copies are preferred, and
	// before the plain HTTP fetch so a fingerprint-blocked
	// publisher page is retried with a browser-shaped TLS
	// handshake before falling through to the catch-all.
	tlsImpersonate := cfg.Providers.Resolution.TLS.Impersonate
	if tlsImpersonate == "" {
		tlsImpersonate = os.Getenv("OKT_FETCH_IMPERSONATE")
	}
	tlsTimeout := cfg.Providers.Resolution.TLS.Timeout
	if tlsTimeout <= 0 {
		tlsTimeout = 30 * time.Second
	}
	if tlsProvider := fetch.NewTLSImpersonationProviderWithFullConfig(
		tlsImpersonate, cfg.Providers.Resolution.Fetch.UserAgent,
		tlsTimeout, fetchRetryCfg, parsers...,
	); tlsProvider != nil {
		resolutionProviders = append(resolutionProviders, tlsProvider)
	}

	resolutionProviders = append(resolutionProviders, fetchResolution)

	// FlareSolverr / headless-browser tier (config-gated,
	// self-disables when no endpoint is configured). Runs
	// last so the cheaper tiers get a chance first; the
	// host-preference store learns to skip them for hosts
	// where FlareSolverr is the only working tier.
	//
	// The tier supports a round-robin pool of endpoints so
	// a single Byparr container is not saturated under burst
	// load: one Chromium queues concurrent requests
	// internally and every queued request burns its 60s
	// timeout, so 50 retrieve_source workers all needing the
	// heavy tier against one container is a timeout storm.
	// Endpoints come from flaresolverr.endpoints (list); the
	// single flaresolverr.url (or FLARESOLVERR_URL env) is
	// appended for backward compatibility. max_concurrency
	// caps in-flight Resolve calls across the pool so
	// workers block cheaply on the semaphore instead of
	// firing requests that just queue inside a container.
	flareCfg := cfg.Providers.Resolution.FlareSolverr
	flareEndpoints := make([]string, 0, len(flareCfg.Endpoints)+1)
	flareEndpoints = append(flareEndpoints, flareCfg.Endpoints...)
	// FLARESOLVERR_ENDPOINTS is a comma-separated env-var
	// alias for flaresolverr.endpoints, mirroring the
	// FLARESOLVERR_URL single-endpoint shorthand. Viper's
	// AutomaticEnv would require the unwieldy
	// PROVIDERS_RESOLUTION_FLARESOLVERR_ENDPOINTS, so we read
	// the short form directly and split on comma. Duplicates
	// and empty entries are filtered by the constructor.
	if envEndpoints := os.Getenv("FLARESOLVERR_ENDPOINTS"); envEndpoints != "" {
		for _, ep := range strings.Split(envEndpoints, ",") {
			if ep = strings.TrimSpace(ep); ep != "" {
				flareEndpoints = append(flareEndpoints, ep)
			}
		}
	}
	flareURL := flareCfg.URL
	if flareURL == "" {
		flareURL = os.Getenv("FLARESOLVERR_URL")
	}
	if flareURL != "" {
		flareEndpoints = append(flareEndpoints, flareURL)
	}
	flareTimeout := flareCfg.Timeout
	flareMaxConcurrency := flareCfg.MaxConcurrency
	if envMax := os.Getenv("FLARESOLVERR_MAX_CONCURRENCY"); envMax != "" {
		if n, err := strconv.Atoi(envMax); err == nil && n > 0 {
			flareMaxConcurrency = n
		}
	}
	if flareProvider := fetch.NewFlareSolverrProviderPool(flareEndpoints, flareTimeout, cfg.Providers.Resolution.Fetch.UserAgent, flareMaxConcurrency, parsers...); flareProvider != nil {
		resolutionProviders = append(resolutionProviders, flareProvider)
	}

	// Build the strategy with static host overrides and SSRF
	// validation. The overrides map pins a host to a provider
	// id (e.g. "www.cell.com" → "flaresolverr") so the strategy
	// tries that provider first for matching hosts, without
	// any learning machinery. The SSRF validator rejects
	// non-http(s) URLs and private/loopback addresses before
	// any provider runs. Both are no-ops when their config is
	// empty.
	fetchStrategy := fetch.NewFetchStrategyWithOverrides(
		resolutionProviders,
		cfg.Providers.Resolution.HostOverrides,
	).WithURLValidator(fetch.ValidateFetchURL)

	// File storage backend. The default is the local filesystem
	// under `var/source_assets` (see configs/config.default.yaml);
	// a future S3 / CDN backend will switch on
	// `providers.storage.backend: "s3"` once the implementation
	// lands. The backend is shared between the retrieve_source
	// worker (which writes images and PDF bodies to it) and the
	// HTTP serving endpoints (which stream them back). A boot
	// error here is fatal: without storage the worker can't
	// persist images and the serving endpoints would 503 on
	// every request.
	storageBackend, err := storage.NewFromConfig(cfg.Providers.Storage)
	if err != nil {
		log.Fatalf("building storage backend: %v", err)
	}
	if fs, ok := storageBackend.(*storage.LocalFileStorage); ok {
		log.Printf("storage: filesystem backend rooted at %s", fs.Root())
	} else {
		log.Printf("storage: backend %q active", cfg.Providers.Storage.Backend)
	}

	// RBAC is a system-side concern (casbin_rule is in okt_system),
	// so it always runs against the system pool. We look up the
	// pool by name from the registry.
	systemPool := registry.Get(cfg.System.Database)
	rbacSvc, err := rbac.SetupRBAC(systemPool.Pool)
	if err != nil {
		log.Fatalf("setting up RBAC: %v", err)
	}
	log.Println("RBAC service initialized")

	// Run one-time data bootstrap steps. Each step is idempotent
	// and only acts when the relevant table is empty, so existing
	// data is never touched. Failures here are fatal because they
	// indicate either a broken database or a misconfigured owner.
	//
	// Order matters: the default admin is seeded before the default
	// repository so a fresh install always has at least one user
	// (and therefore at least one valid owner) by the time the
	// repository bootstrap runs. The default repository bootstrap
	// is also called lazily from GET /repositories, so a first
	// user that registers before this code runs still gets a
	// starter repo owned by them.
	if _, err := bootstrap.EnsureDefaultAdmin(ctx, registry, cfg, rbacSvc); err != nil {
		log.Fatalf("bootstrapping default admin: %v", err)
	}
	// The default-repository bootstrap is deferred to after the
	// HTTP handler + provider registry + ontology source are
	// wired (further down in this function) so it can seed the
	// freshly-created default repo's per-repository settings
	// (providers + contexts) via the same handler.SeedDefaultRepositorySettings
	// path CreateRepository uses. Running it here, before the
	// handler is built, would leave the default repo with no
	// settings rows — which the search/retrieve/extract gates
	// deny ("search provider not enabled for this repository").

	// Build the background-task manager. It uses the same
	// provider instances as the API and connects to the task
	// database named by cfg.Task.Database (which the registry
	// already opened and migrated). The task manager owns its
	// own client lifecycle on top of the registry's pool. The
	// manager also gets the default-pool *store.Queries and
	// the registry so the RetrieveSource worker can resolve
	// the per-repo database for a given repository and write
	// a row into okt_repository.sources.
	taskPool := registry.Get(cfg.Task.Database)

	aiProviders := make(map[string]ai.AIProvider)

	ollamaModels := ai.ModelsForProvider(cfg, "ollama")
	ollamaBaseURL := cfg.Providers.AI.Ollama.BaseURL
	if ollamaBaseURL == "" {
		ollamaBaseURL = os.Getenv("OLLAMA_BASE_URL")
	}
	if ollamaBaseURL != "" {
		// Apply the env-var override on a config copy so
		// NewOllamaProviderFromConfig picks up both the resolved
		// base_url and the configured http_timeout.
		ollamaCfg := cfg.Providers.AI.Ollama
		ollamaCfg.BaseURL = ollamaBaseURL
		aiProviders["ollama"] = ai.NewOllamaProviderFromConfig(ollamaCfg, ollamaModels)
	}

	ollamaCloudModels := ai.ModelsForProvider(cfg, "ollama_cloud")
	ollamaCloudKey := cfg.Providers.AI.OllamaCloud.APIKey
	if ollamaCloudKey == "" {
		ollamaCloudKey = os.Getenv("OLLAMA_API_KEY")
	}
	if ollamaCloudKey != "" {
		ocCfg := cfg.Providers.AI.OllamaCloud
		ocCfg.APIKey = ollamaCloudKey
		aiProviders["ollama_cloud"] = ai.NewOllamaCloudProviderFromConfig(ocCfg, ollamaCloudModels)
	}

	openrouterModels := ai.ModelsForProvider(cfg, "openrouter")
	openrouterKey := cfg.Providers.AI.OpenRouter.APIKey
	if openrouterKey == "" {
		openrouterKey = os.Getenv("OPENROUTER_API_KEY")
	}
	if openrouterKey != "" {
		// Build a config copy with the resolved key so
		// NewOpenRouterProviderFromConfig sees the env-var
		// fallback (the YAML api_key is usually empty in dev).
		orCfg := cfg.Providers.AI.OpenRouter
		orCfg.APIKey = openrouterKey
		aiProviders["openrouter"] = ai.NewOpenRouterProviderFromConfig(orCfg, openrouterModels)
	}

	// Wrap each AI provider in a per-model rate limiter so the
	// high-fan-out task queues (summarize_concepts: 100, extract_concepts:
	// 100, …) can't drown an LLM provider. The decorator keys on
	// req.Model at call time, so a provider serving many models gets
	// one bucket per model with rate_limit_rpm from the catalog
	// (default 30). Providers with no limited models pass through
	// unchanged (zero overhead). See internal/providers/ai/ratelimit.go.
	for id, p := range aiProviders {
		aiProviders[id] = ai.MaybeWrapRateLimited(p, id, cfg)
	}

	// Apply the shared LLM retry config (cfg.Providers.LLMRetry) to
	// the four phase-provider packages' defaultRetryConfig vars.
	// Each package's SetRetryDefaults overrides its package var so
	// every retryWithBackoff call (fact_extraction, concept_extraction,
	// image_fact_extraction, summarization, refinement, synthesis)
	// picks up the operator's MaxAttempts/BaseDelay/MaxDelay/PerCallTO
	// without each provider needing a constructor change. Defaults:
	// 4 attempts, 2s base, 30s cap, 5m per-call — chosen so the
	// worker LLM timeouts (20-25m) comfortably exceed the retry
	// budget (4 × 5m + backoffs ≈ 20m) and the retry loop can
	// actually complete its 4 attempts instead of being killed by
	// the worker's outer ctx (the historical 120s value that
	// severed 95,627 facts).
	decomposition.SetRetryDefaults(cfg.Providers.LLMRetry)
	summarization.SetRetryDefaults(cfg.Providers.LLMRetry)
	refinement.SetRetryDefaults(cfg.Providers.LLMRetry)
	synthesis.SetRetryDefaults(cfg.Providers.LLMRetry)

	chunkingProviders := map[string]decomposition.ChunkingProvider{
		"simple": decomposition.NewSimpleChunkingProvider(
			cfg.Providers.Decomposition.Chunking.ChunkSize,
			cfg.Providers.Decomposition.Chunking.ChunkOverlap,
		),
		"sentence": decomposition.NewSentenceChunkingProvider(0),
	}

	// Fact extraction provider: a single AI-backed wrapper, the
	// one the worker will actually use. The id comes from
	// cfg.Providers.Decomposition.FactExtraction.Provider so a
	// deployment with multiple AI providers (ollama + openrouter)
	// can route fact extraction to one of them and surface the
	// others on the AI tab without cluttering the Decomposition
	// tab. A missing AI provider at the id the config names
	// means "not configured" (the worker logs and skips) rather
	// than a crash, so a partially-wired deployment still boots.
	dcCfg := cfg.Providers.Decomposition.FactExtraction
	factExtractors := make(map[string]decomposition.FactExtractionProvider)
	if dcCfg.Provider != "" {
		if aiProv, ok := aiProviders[dcCfg.Provider]; ok && aiProv != nil {
			factExtractors[dcCfg.Provider] = decomposition.NewAIFactExtractionProvider(aiProv, dcCfg.Model)
		} else {
			log.Printf("decomposition: fact extraction provider %q not configured (no AI provider registered under that id); fact extraction will be a no-op", dcCfg.Provider)
		}
	} else {
		log.Printf("decomposition: fact_extraction.provider is empty; fact extraction will be a no-op")
	}

	// Image fact extraction provider: a multimodal AI-backed wrapper
	// that sends each source image (inline image fetched via the
	// fetch provider, or PDF page render bytes) to a vision-capable
	// model together with the source URL/title/alt text. Mirrors
	// the text extractor wiring: a single instance keyed by the
	// configured provider id. The fetch provider supplies
	// FetchImageBytes for inline image URLs. A missing AI provider
	// leaves the map empty so the worker logs "not configured" and
	// skips images (text facts still produced).
	imgCfg := cfg.Providers.Decomposition.ImageExtraction
	imageExtractors := make(map[string]decomposition.ImageFactExtractionProvider)
	if imgCfg.Provider != "" {
		if aiProv, ok := aiProviders[imgCfg.Provider]; ok && aiProv != nil {
			imageExtractors[imgCfg.Provider] = decomposition.NewAIImageFactExtractionProvider(aiProv, imgCfg.Model, fetchResolution, imgCfg.MaxImageBytes)
		} else {
			log.Printf("decomposition: image extraction provider %q not configured (no AI provider registered under that id); image extraction will be a no-op", imgCfg.Provider)
		}
	} else {
		log.Printf("decomposition: image_extraction.provider is empty; image extraction will be a no-op")
	}

	// Embedding provider: reuse the same provider instances the
	// `ai` block already built (ollama / ollama_cloud / openrouter).
	// The embedding provider name must match a key in aiProviders;
	// when it doesn't (or when the named provider doesn't implement
	// ai.EmbeddingProvider) embeddingProvider stays nil and the
	// embed_facts worker logs "not configured" rather than crashing
	// — a deployment that hasn't wired an embedding model still
	// boots and serves facts (just without dedup).
	var embeddingProvider ai.EmbeddingProvider
	if embProv, ok := aiProviders[cfg.Providers.Embedding.Provider]; ok && embProv != nil {
		if ep, ok := embProv.(ai.EmbeddingProvider); ok {
			embeddingProvider = ep
		} else {
			log.Printf("embedding: provider %q does not implement ai.EmbeddingProvider; embed_facts will be a no-op", cfg.Providers.Embedding.Provider)
		}
	} else {
		log.Printf("embedding: provider %q not configured (ai block missing key); embed_facts will be a no-op", cfg.Providers.Embedding.Provider)
	}

	// Concept extraction provider: a chat model that extracts
	// (concept, context, seed_aliases) triples from a stable fact.
	// The context is constrained to the embedded context vocabulary
	// the worker loads at boot (see ontologySource below).
	// When concept_extraction.enabled is false or the provider/model
	// is not configured, conceptExtractor stays nil and the
	// extract_concepts worker is a no-op (the dedup→cleanup chain
	// still runs, just without concept linking).
	var conceptExtractor decomposition.ConceptExtractionProvider
	if ccCfg := cfg.Providers.Decomposition.ConceptExtraction; ccCfg.Enabled {
		if aiProv, ok := aiProviders[ccCfg.Provider]; ok && aiProv != nil {
			conceptExtractor = decomposition.NewAIConceptExtractionProvider(aiProv, ccCfg.Model)
		} else {
			log.Printf("concept_extraction: provider %q not configured (no AI provider registered under that id); extract_concepts will be a no-op", ccCfg.Provider)
		}
	} else {
		log.Printf("concept_extraction: not enabled; extract_concepts will be a no-op")
	}

	// Concept refinement provider: resolves unresolved concept
	// candidates — proposes the full formal canonical name, known
	// aliases to add, and aliases to prune. Runs once per unresolved
	// candidate (genuinely new concepts only; resolved candidates route
	// via cache, matched candidates route via pre-LLM DB lookups). A
	// missing provider/model leaves refiner nil so refine_concepts is
	// a no-op.
	var refiner refinement.RefineProvider
	if refCfg := cfg.Providers.Refinement; refCfg.Enabled {
		if aiProv, ok := aiProviders[refCfg.Provider]; ok && aiProv != nil {
			refiner = refinement.NewAIRefineProvider(aiProv, refCfg.Model)
		} else {
			log.Printf("refinement: provider %q not configured; refine_concepts will be a no-op", refCfg.Provider)
		}
	}

	// Concept summarization provider: a chat model that receives the
	// facts linked to a (concept, context) pair and returns a
	// credulous markdown summary citing central/key facts via
	// [text](<fact_id>). The summarize_concepts task fans out from
	// extract_concepts in parallel with embed_concepts. When enabled
	// is false (or the provider/model is not configured), the
	// summarizer stays nil and the summarize_concepts worker is a
	// no-op (extract_concepts does not enqueue it).
	var summarizer summarization.SummarizationProvider
	if sumCfg := cfg.Providers.Summarization; sumCfg.Enabled {
		if aiProv, ok := aiProviders[sumCfg.Provider]; ok && aiProv != nil {
			summarizer = summarization.NewAISummarizationProvider(aiProv, sumCfg.Model)
		} else {
			log.Printf("summarization: provider %q not configured (no AI provider registered under that id); summarize_concepts will be a no-op", sumCfg.Provider)
		}
	} else {
		log.Printf("summarization: not enabled; summarize_concepts will be a no-op")
	}

	// Concept synthesis provider: a chat model that folds ALL of a
	// canonical-name group's summary slices into ONE authoritative
	// "definition" per (repository_id, lower(canonical_name)). The
	// synthesize_concept task is chained from summarize_concepts —
	// every time a slice is written/updated, one synthesize_concept
	// job is enqueued for that concept_id. A separate image-picker
	// model narrows the group's image facts before the synthesis call
	// so the definition can embed illustrative images. When enabled
	// is false (or the provider/model is not configured), the
	// synthesizer stays nil and the synthesize_concept worker is a
	// no-op (summarize_concepts does not enqueue it).
	var synthesizer synthesis.SynthesisProvider
	if synCfg := cfg.Providers.Synthesis; synCfg.Enabled {
		if aiProv, ok := aiProviders[synCfg.Provider]; ok && aiProv != nil {
			synthesizer = synthesis.NewAISynthesisProvider(aiProv, synCfg.Model, synCfg.ImagePickerModelOr(synCfg.Model))
		} else {
			log.Printf("synthesis: provider %q not configured (no AI provider registered under that id); synthesize_concept will be a no-op", synCfg.Provider)
		}
	} else {
		log.Printf("synthesis: not enabled; synthesize_concept will be a no-op")
	}

	// Autocite posture classifier: a chat model that labels each
	// (report sentence, candidate fact) pair as related / supports /
	// contradicts / irrelevant so the annotate_report worker can drop
	// irrelevant matches before persisting report_annotations. When
	// the provider/model is not configured (or Enabled is false) the
	// classifier stays nil and the worker falls back to the legacy
	// keep-all behavior (posture = NULL). A per-repo
	// repository_model_settings row for task_kind='report_annotation'
	// can override the model id at runtime via ModelResolver.
	var postureClassifier posture.Classifier
	if pcCfg := cfg.Providers.Reports.PostureClassifier; pcCfg.Enabled {
		if pcCfg.Provider != "" {
			if aiProv, ok := aiProviders[pcCfg.Provider]; ok && aiProv != nil {
				postureClassifier = posture.NewAIClassifier(aiProv, pcCfg.Model)
			} else {
				log.Printf("posture_classifier: provider %q not registered; annotate_report falls back to keep-all (posture = NULL)", pcCfg.Provider)
			}
		} else {
			log.Printf("posture_classifier: no provider set; annotate_report falls back to keep-all (posture = NULL)")
		}
	} else {
		log.Printf("posture_classifier: not enabled; annotate_report falls back to keep-all (posture = NULL)")
	}

	// Claim extractor: a chat model that reads each report sentence
	// and emits the verifiable assertions it makes (numeric values,
	// causal claims, comparisons, quotations, definitions). The
	// annotate_report worker uses the extracted claims to drive an
	// additional retrieval pass per claim so the posture classifier
	// sees facts that match the sentence's SPECIFIC assertion, not
	// just its broad topic. Uses the same DeepSeek V4 Flash model as
	// the posture classifier (claim extraction is a tight instruction-
	// following JSON-emission task where DeepSeek's instruction-tuned
	// head is the stronger choice). When the provider/model is not
	// configured (or Enabled is false) the extractor stays nil and
	// the worker falls back to embedding-only retrieval. A per-repo
	// repository_model_settings row for task_kind='claim_extraction'
	// can override the model id at runtime via ModelResolver.
	var claimExtractor claims.Extractor
	if ceCfg := cfg.Providers.Reports.ClaimExtractor; ceCfg.Enabled {
		if ceCfg.Provider != "" {
			if aiProv, ok := aiProviders[ceCfg.Provider]; ok && aiProv != nil {
				claimExtractor = claims.NewAIClaimExtractor(aiProv, ceCfg.Model)
			} else {
				log.Printf("claim_extractor: provider %q not registered; claim-driven retrieval disabled", ceCfg.Provider)
			}
		} else {
			log.Printf("claim_extractor: no provider set; claim-driven retrieval disabled")
		}
	} else {
		log.Printf("claim_extractor: not enabled; claim-driven retrieval disabled")
	}

	// Ontology source: the curated context vocabulary the concept-
	// extraction prompt offers the model as the allowed context
	// labels. The server uses the embedded contexts.json snapshot
	// (committed alongside the binary, go:embedded at build time).
	// There is no live SPARQL fetch — the file is the single source
	// of truth. An operator refreshes it by editing the file and
	// redeploying (the experiments scripts in scripts/experiments/
	// can help derive a new list). A boot-time parse failure is
	// fatal (the file is malformed).
	ontologySource, err := ontology.NewEmbeddedL3Source()
	if err != nil {
		log.Fatalf("ontology: embedded source: %v", err)
	}
	if classes, err := ontologySource.ContextClasses(context.Background()); err == nil {
		log.Printf("ontology: using embedded context vocabulary (%d categories)", len(classes))
	} else {
		log.Fatalf("ontology: embedded source unreadable: %v", err)
	}

	// Qdrant store: build the gRPC client and ensure the collection
	// exists at the configured dimension before starting the task
	// manager. A missing Qdrant host is a hard error in production
	// (the embedding+dedup pipeline is non-functional without it);
	// tests that don't need Qdrant leave cfg.Providers.Qdrant.Host
	// empty and NewClient returns an error, which we turn into a
	// nil store so the taskmanager workers degrade gracefully
	// (they log "not configured" and skip). The wiring below keeps
	// the API server booting so non-facts endpoints still serve.
	//
	// QDRANT_HOST / QDRANT_PORT env vars override the YAML the same
	// way OPENROUTER_API_KEY does — the docker-compose wires it
	// this way so secrets/hosts stay out of the YAML. The
	// AutomaticEnv replacer would look for PROVIDERS_QDRANT_HOST,
	// which is not the public name, so we read the legacy short
	// form here.
	if v := os.Getenv("QDRANT_HOST"); v != "" {
		cfg.Providers.Qdrant.Host = v
	}
	if v := os.Getenv("QDRANT_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Providers.Qdrant.Port = n
		}
	}
	if v := os.Getenv("QDRANT_API_KEY"); v != "" {
		cfg.Providers.Qdrant.APIKey = v
	}
	var qdrantStore *qdrantstore.Store
	if cfg.Providers.Qdrant.Host != "" {
		qs, err := qdrantstore.NewClient(cfg.Providers.Qdrant)
		if err != nil {
			log.Fatalf("building qdrant client: %v", err)
		}
		// Health-check before EnsureCollection so a misconfigured
		// host fails at boot instead of on the first upsert.
		hcCtx, hcCancel := context.WithTimeout(ctx, 5*time.Second)
		if _, err := qs.HealthCheck(hcCtx); err != nil {
			hcCancel()
			log.Fatalf("qdrant health check failed (host %s:%d): %v; set providers.qdrant.host to empty to boot without the embedding+dedup pipeline",
				cfg.Providers.Qdrant.Host, cfg.Providers.Qdrant.Port, err)
		}
		hcCancel()
		if err := qs.EnsureCollection(ctx, cfg.Providers.Embedding.Dimensions); err != nil {
			log.Fatalf("ensuring qdrant collection: %v", err)
		}
		// Ensure the concept collection exists alongside the facts
		// collection. Same dimensions (concepts reuse the embedding
		// config); separate collection so concept searches don't scan
		// fact vectors and a dimension change on one doesn't force a
		// re-embedding of the other.
		if err := qs.EnsureConceptCollection(ctx, cfg.Providers.Embedding.Dimensions); err != nil {
			log.Fatalf("ensuring qdrant concept collection: %v", err)
		}
		qdrantStore = qs
	} else {
		log.Printf("qdrant: providers.qdrant.host is empty; embedding+dedup pipeline disabled (facts endpoints still serve)")
	}

	modelResolver := tasks.NewModelResolver(cfg, aiProviders, queries)
	// Build the promptset resolver: a DB provider over the system
	// pool + the built-in provider, chained. The resolver is the
	// single source of truth for "which promptsets exist" — the
	// HTTP handler uses it to validate hashes, the workers use it
	// to resolve a repo's effective philosophy. Nil-safe: a
	// deployment that hasn't wired the system pool still gets the
	// built-in promptset via the BuiltinProvider.
	promptsetResolver := promptset.NewResolver(promptset.NewDBProvider(queries))
	tm, err := taskmanager.New(ctx, cfg, taskPool, queries, registry, searchProviders, fetchStrategy, chunkingProviders, factExtractors, imageExtractors, conceptExtractor, refiner, embeddingProvider, qdrantStore, storageBackend, summarizer, synthesizer, postureClassifier, claimExtractor, modelResolver, promptsetResolver)
	if err != nil {
		log.Fatalf("setting up task manager: %v", err)
	}

	h := api.NewHandler(queries, cfg, rbacSvc, systemPool.Pool, registry, audit.NewPostgresRecorder(systemPool.Pool))
	h.SetSource(handler.NewSource(searchProviders, fetchStrategy, chunkingProviders, factExtractors, imageExtractors, storageBackend, parsers))
	// Wire the live provider registry (built from the same maps
	// passed to NewSource) so the per-repository settings feature
	// can seed + gate against the actual live provider set. The
	// registry is the single source of truth for which provider ids
	// exist in this deployment — nothing about the live set is
	// hardcoded.
	h.SetProviderRegistry(handler.NewProviderRegistry(searchProviders, fetchStrategy))
	// Wire the embedded context vocabulary so CreateRepository can
	// seed repository_contexts with the full label set.
	h.SetOntologySource(ontologySource)
	// Now that the handler, provider registry, and ontology
	// source are all wired, run the startup-time default-repository
	// bootstrap with the settings seeder. The seeder uses the same
	// default-preset resolution path as CreateRepository, so the
	// freshly-created default repo gets every live provider enabled
	// and the full context vocabulary seeded — the search/retrieve
	// gates pass out of the box. The lazy path (re-bound in
	// SetOntologySource) handles the case where the startup call
	// skipped because no users existed yet.
	if _, err := bootstrap.EnsureDefaultRepository(ctx, registry, cfg, "", h.Deps().DefaultSettingsSeeder); err != nil {
		log.Fatalf("bootstrapping default repository: %v", err)
	}
	h.SetStorage(handler.NewStorage(storageBackend))
	h.SetTaskEnqueuer(tm.Enqueuer())
	h.SetMigrateEnqueuer(tm.MigrateEnqueuer())
	h.SetRegistrySyncEnqueuer(tm.RegistrySyncEnqueuer())
	h.SetAI(handler.NewAI(aiProviders, embeddingProvider, cfg.Providers.Embedding, queries))
	// Wire the qdrant vector store + embedding provider into the
	// handler deps so the fact/concept search endpoints can run
	// the hybrid (lexical + semantic via RRF) path. Both are
	// nil-safe: when Qdrant or the embedding provider is not
	// configured at boot, the search endpoints degrade to
	// lexical-only. The same qdrantStore + embeddingProvider
	// instances are reused by the taskmanager (embed_facts /
	// dedup workers) so there is one connection per process.
	h.SetQdrant(qdrantStore)
	h.SetEmbeddingProvider(embeddingProvider)
	// Wire the tasks handler bundle so /api/v1/tasks/* serves
	// job-list and job-get responses. The bundle is split out
	// from the rest of NewHandler (which has no River client)
	// because the River client only exists once the task manager
	// is built. Without this, /api/v1/tasks/* returns 503
	// (the "notConfigured" fallback in sourceRoutes /
	// tasksRoutes) because the bundle is nil.
	h.SetTasks(handler.NewTasks(tm.Client(), taskPool.Pool, cfg.Task.Queues))

	// Wire the remote-registry handler. The registry client map is
	// built from the resolved `providers.registries` config (the
	// legacy single `providers.registry` block is synthesized as the
	// "default" entry). The map is shared by the HTTP remote handler
	// and the task workers so both resolve the same per-repo client
	// from the repo's `registry_id` column. When no registry is
	// configured the map is empty and every registry feature is a
	// no-op. The same ClientMap was built at the top of runAPI so
	// the registry search provider could join the searchProviders
	// map; reuse it here instead of rebuilding.
	h.SetRemote(handler.NewRemote(registryClients, cfg.Providers))
	h.SetRegistryClients(registryClients)
	// Build the registry cache provider (the cache-hit shortcut the
	// retrieve_source / pull workers call instead of inlining
	// SearchSource + PullSource + PullDecomposition + filter). It
	// is wired into the task manager workers below alongside the
	// ClientMap; the HTTP layer doesn't need it directly.
	registryCacheProvider := registryclient.NewRegistryCacheProvider(registryServices)
	_ = registryCacheProvider
	h.SetModelCatalog(handler.NewModelCatalog(cfg.Providers.AI.Models))
	h.SetPromptsetResolver(promptsetResolver)
	h.SetRemoteDedupEnqueuer(tm.RemoteDedupEnqueuer())
	h.SetRemotePullBatchEnqueuer(tm.RemotePullBatchEnqueuer())

	// OAuth 2.1 authorization server + MCP server. The OAuth
	// server lets MCP clients (Claude Desktop, etc.) connect to
	// the OKT MCP endpoint via Authorization Code + PKCE instead
	// of a static API token. The access tokens are HS256 JWTs
	// signed with cfg.Auth.JWTSecret (shared with the session
	// JWT) so the OAuthBearer middleware validates them
	// statelessly.
	//
	// The MCP handler runs in-process: the three tools call
	// store.Queries + rbac directly, not the REST API, so a tool
	// call is one DB round-trip, not an HTTP self-call. The
	// per-call repository resolver reuses the same
	// RepoDBCache + SlugCache the per-repo chi routes use, so
	// UUID-or-slug resolution is consistent across the two
	// surfaces.
	oauthIssuer := cfg.OAuth.Issuer
	if oauthIssuer == "" {
		oauthIssuer = fmt.Sprintf("http://localhost:%d", cfg.Server.Port)
	}
	oauthCfg := oauth.Config{
		Issuer:          oauthIssuer,
		AccessTokenTTL:  cfg.OAuth.AccessTokenTTL,
		RefreshTokenTTL: cfg.OAuth.RefreshTokenTTL,
		AuthCodeTTL:     cfg.OAuth.AuthCodeTTL,
	}
	if oauthCfg.AccessTokenTTL == 0 {
		oauthCfg.AccessTokenTTL = 15 * time.Minute
	}
	if oauthCfg.RefreshTokenTTL == 0 {
		oauthCfg.RefreshTokenTTL = 30 * 24 * time.Hour
	}
	if oauthCfg.AuthCodeTTL == 0 {
		oauthCfg.AuthCodeTTL = 10 * time.Minute
	}
	oauthServer := oauth.NewServer(oauthCfg, cfg.Auth.JWTSecret, queries, oauth.DefaultUserLookup(queries))
	h.SetOAuth(handler.NewOAuth(oauthServer, oauthIssuer, oauthIssuer+"/api/v1/mcp"))
	// The login-cookie helper needs the same symmetric secret to
	// sign its HMAC; set it once at wiring time. Reuses
	// cfg.Auth.JWTSecret so there's one secret to rotate.
	handler.SetLoginCookieSecret(cfg.Auth.JWTSecret)
	mcpHandler := handler.NewMCP(h.Deps(), handler.ResolveRepoPoolFromCaches(registry, h.RepoDBCache(), h.SlugCache()))
	mcpHandler.SetTaskEnqueuer(tm.Enqueuer())
	mcpHandler.SetTaskClient(tm.Client())
	mcpHandler.SetTaskPool(taskPool.Pool)
	mcpHandler.SetSearchProviders(searchProviders)
	mcpHandler.SetDefaultSearchProvider(defaultSearchProvider)
	h.SetMCP(mcpHandler)
	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:      h.Router(),
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	// Start the task manager in its own goroutine so it can run
	// workers in parallel with the HTTP server. River.Start blocks
	// until the context is cancelled or an error occurs.
	tmDone := make(chan struct{})
	go func() {
		defer close(tmDone)
		if err := tm.Start(ctx); err != nil {
			log.Printf("task manager exited: %v", err)
		}
	}()

	go func() {
		log.Printf("API server listening on :%d", cfg.Server.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down API server...")
	srv.Shutdown(context.Background())

	if err := tm.Stop(context.Background()); err != nil {
		log.Printf("task manager stop error: %v", err)
	}
	<-tmDone
	log.Println("API server stopped")
}
