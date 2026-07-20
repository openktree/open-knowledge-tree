const BASE = "/api/v1";

let onUnauthorized = null;

export function setUnauthorizedCallback(fn) {
  onUnauthorized = fn;
}

function getToken() {
  return localStorage.getItem("token");
}

function getRepoID() {
  return localStorage.getItem("repository_id") || "";
}

// authedFetch is the blob-fetch variant of `request`: it sends
// the same Authorization / X-Repository-ID headers but skips the
// JSON parse so binary responses (images, PDFs) can be turned into
// object URLs by the caller. Used by the source-asset serving
// endpoints (api.getSourceImage / api.getSourceBody), which the
// plain `<img src>` tag can't hit because browsers don't send the
// Authorization header on `<img>` requests.
//
// Returns the raw `Response` so the caller can read bytes, check
// `Content-Type`, or short-circuit on 404 (the caller may want to
// fall back to a remote URL rather than throw). Throws on network
// errors and non-2xx/non-404 statuses.
async function authedFetch(path) {
  const token = getToken();
  const repoID = getRepoID();
  const headers = {};
  if (token) headers["Authorization"] = `Bearer ${token}`;
  if (repoID) headers["X-Repository-ID"] = repoID;

  const res = await fetch(`${BASE}${path}`, { headers });
  if (res.status === 404) return null;
  if (!res.ok) {
    let errorMessage = "request failed";
    try {
      const data = await res.json();
      errorMessage = data.error || errorMessage;
    } catch {
      // Binary / non-JSON error body — keep the generic message.
    }
    if (res.status === 401 && onUnauthorized) onUnauthorized();
    throw new Error(errorMessage);
  }
  return res;
}

async function request(path, options = {}) {
  const token = getToken();
  const repoID = getRepoID();
  const headers = { "Content-Type": "application/json", ...options.headers };
  if (token) {
    headers["Authorization"] = `Bearer ${token}`;
  }
  if (repoID) {
    headers["X-Repository-ID"] = repoID;
  }

  const res = await fetch(`${BASE}${path}`, { ...options, headers });
  let data;
  try {
    data = await res.json();
  } catch (e) {
    if (!res.ok) {
      throw new Error(`request failed (${res.status})`);
    }
    throw e;
  }

  if (!res.ok) {
    const errorMessage = data?.error || `request failed (${res.status})`;
    if (res.status === 401 && onUnauthorized) {
      onUnauthorized();
    }
    throw new Error(errorMessage);
  }

  return data;
}

export const api = {
  register(body) {
    return request("/auth/register", { method: "POST", body: JSON.stringify(body) });
  },

  login(body) {
    return request("/auth/login", { method: "POST", body: JSON.stringify(body) });
  },

  logout() {
    return request("/auth/logout", { method: "POST" });
  },

  getMe() {
    return request("/users/me");
  },

  getMyPermissions() {
    return request("/permissions");
  },

  listProviders() {
    return request("/sources/providers");
  },

  listAIProviders() {
    return request("/ai/providers");
  },

  listEmbeddingProviders() {
    return request("/ai/embedding/providers");
  },

  listDecompositionProviders() {
    return request("/sources/decomposition/providers");
  },

  testSearch(provider, query, { repository_id = "", cursor = "", per_page = 0 } = {}) {
    return request(`/sources/${encodeURIComponent(provider)}/search`, {
      method: "POST",
      body: JSON.stringify({ query, repository_id, cursor, per_page }),
    });
  },

  classifyResource(url) {
    return request("/sources/classify", { method: "POST", body: JSON.stringify({ url }) });
  },

  retrieveSource(url, repoID, process = false, doi = "") {
    return request("/sources/retrieve", {
      method: "POST",
      body: JSON.stringify({ url, repository_id: repoID || "", process, doi }),
    });
  },

  listSources(slug, { q = "", limit = 100, offset = 0 } = {}) {
    const qs = new URLSearchParams({ limit, offset });
    if (q) qs.set("q", q);
    return request(`/repositories/${slug}/sources?${qs.toString()}`);
  },

  getSource(slug, sourceID) {
    return request(`/repositories/${slug}/sources/${sourceID}`);
  },

  // getSourceImage fetches the stored bytes of a single source
  // image and returns an object URL suitable for `<img src>`. The
  // caller MUST revoke the URL when it's done with it (e.g. on
  // unmount) to avoid leaking blob memory. Returns null when the
  // image has not been mirrored yet (storage_key is NULL on the
  // server) so the caller can fall back to the remote `url`.
  async getSourceImage(slug, sourceID, imageID) {
    const res = await authedFetch(`/repositories/${slug}/sources/${sourceID}/images/${imageID}`);
    if (!res) return null;
    const blob = await res.blob();
    return URL.createObjectURL(blob);
  },

  // getSourceBody fetches the stored full source body (PDF) and
  // returns an object URL. Same contract as getSourceImage:
  // returns null when the body has not been stored.
  async getSourceBody(slug, sourceID) {
    const res = await authedFetch(`/repositories/${slug}/sources/${sourceID}/body`);
    if (!res) return null;
    const blob = await res.blob();
    return URL.createObjectURL(blob);
  },

  deleteSource(slug, sourceID) {
    return request(`/repositories/${slug}/sources/${sourceID}`, { method: "DELETE" });
  },

  // uploadSourceFile POSTs a multipart/form-data upload (PDF/HTML/MD/TXT)
  // to the per-repo /sources/upload endpoint. The server parses the file
  // in-process and enqueues decomposition. When invID is supplied, the
  // source is atomically linked to that investigation. Returns the parsed
  // JSON response {job_id, source_id, status, investigation_linked}.
  async uploadSourceFile(slug, file, kind = "", invID = "") {
    const form = new FormData();
    form.append("file", file);
    if (kind) form.append("kind", kind);
    if (invID) form.append("investigation_id", invID);

    const token = getToken();
    const headers = {};
    if (token) headers["Authorization"] = `Bearer ${token}`;
    // Do NOT set Content-Type — the browser sets it with the boundary.

    const res = await fetch(`${BASE}/repositories/${slug}/sources/upload`, {
      method: "POST",
      headers,
      body: form,
    });
    const data = await res.json();
    if (!res.ok) {
      throw new Error(data.error || "upload failed");
    }
    return data;
  },

  // uploadSourceText POSTs raw text (or markdown) as JSON to the same
  // /sources/upload endpoint. The server treats the text as already-
  // structured content (markdown is detected heuristically) and
  // enqueues decomposition. When invID is supplied, the source is
  // atomically linked to that investigation.
  uploadSourceText(slug, text, title = "", kind = "", invID = "") {
    const body = { text };
    if (title) body.title = title;
    if (kind) body.kind = kind;
    if (invID) body.investigation_id = invID;
    return request(`/repositories/${slug}/sources/upload`, {
      method: "POST",
      body: JSON.stringify(body),
    });
  },

  listUsers() {
    return request("/admin/users");
  },

  assignRole(body) {
    return request("/admin/users/roles", { method: "PUT", body: JSON.stringify(body) });
  },

  removeRole(body) {
    return request("/admin/users/roles", { method: "DELETE", body: JSON.stringify(body) });
  },

  listPermissions() {
    return request("/admin/permissions");
  },

  listRepositoryDatabases() {
    return request("/admin/databases");
  },

  listRepositories() {
    return request("/repositories");
  },

  getRepository(repoID) {
    return request(`/repositories/${repoID}`);
  },

  createRepository(body) {
    return request("/repositories", { method: "POST", body: JSON.stringify(body) });
  },

  updateRepository(repoID, body) {
    return request(`/repositories/${repoID}`, { method: "PUT", body: JSON.stringify(body) });
  },

  deleteRepository(repoID) {
    return request(`/repositories/${repoID}`, { method: "DELETE" });
  },

  getRepositoryPermissions(repoID) {
    return request(`/repositories/${repoID}/permissions`);
  },

  // Repository presets (the "type" dropdown on the create form).
  listRepositoryPresets() {
    return request("/repositories/presets");
  },

  // Per-repository settings (repo-admin surface).
  getRepositorySettings(repoID) {
    return request(`/repositories/${repoID}/settings`);
  },

  setRepositoryProvider(repoID, body) {
    return request(`/repositories/${repoID}/settings/providers`, {
      method: "PUT",
      body: JSON.stringify(body),
    });
  },

  addRepositoryContext(repoID, body) {
    return request(`/repositories/${repoID}/settings/contexts`, {
      method: "POST",
      body: JSON.stringify(body),
    });
  },

  updateRepositoryContext(repoID, context, body) {
    return request(`/repositories/${repoID}/settings/contexts/${encodeURIComponent(context)}`, {
      method: "PUT",
      body: JSON.stringify(body),
    });
  },

  migrateRepositoryContext(repoID, context, body) {
    return request(
      `/repositories/${repoID}/settings/contexts/${encodeURIComponent(context)}/migrate`,
      {
        method: "POST",
        body: JSON.stringify(body),
      },
    );
  },

  deleteRepositoryContext(repoID, context) {
    return request(`/repositories/${repoID}/settings/contexts/${encodeURIComponent(context)}`, {
      method: "DELETE",
    });
  },

  // Context mappings (local ↔ registry) — see migration 0038.
  getRepositoryContextMappings(repoID) {
    return request(`/repositories/${repoID}/settings/context-mappings`);
  },

  setRepositoryContextMapping(repoID, body) {
    return request(`/repositories/${repoID}/settings/context-mappings`, {
      method: "PUT",
      body: JSON.stringify(body),
    });
  },

  deleteRepositoryContextMapping(repoID, localContext) {
    return request(
      `/repositories/${repoID}/settings/context-mappings/${encodeURIComponent(localContext)}`,
      {
        method: "DELETE",
      },
    );
  },

  setUnmappedContextPolicy(repoID, body) {
    return request(`/repositories/${repoID}/settings/unmapped-policy`, {
      method: "PUT",
      body: JSON.stringify(body),
    });
  },

  contributeAll(repoID) {
    return request(`/repositories/${repoID}/settings/contribute-all`, {
      method: "POST",
    });
  },

  pullAllFromRegistry(repoID) {
    return request(`/repositories/${repoID}/settings/pull-all`, {
      method: "POST",
    });
  },

  setAutoContribute(repoID, enabled) {
    return request(`/repositories/${repoID}/settings/auto-contribute`, {
      method: "PUT",
      body: JSON.stringify({ enabled }),
    });
  },

  // Update the per-repo registry selector + on/off toggle. Body
  // fields are optional: { registry_id?, enabled?, allowed_models? }.
  // When `enabled` is true, `registry_id` (if provided) must be in the
  // configured registries list. `allowed_models` is the per-repo model
  // whitelist for the registry cache (null = inherit global, array =
  // per-repo override).
  setRepositoryRegistry(repoID, body) {
    return request(`/repositories/${repoID}/settings/registry`, {
      method: "PUT",
      body: JSON.stringify(body),
    });
  },

  // Update the per-repo push/pull sync levels. Body fields are
  // optional: { push_level?, pull_level? }. Each must be "facts" or
  // "concepts" (case-insensitive). "facts" = sources + facts only;
  // "concepts" = adds concepts, links, concept embeddings (default).
  setRepositorySyncLevels(repoID, body) {
    return request(`/repositories/${repoID}/settings/sync-levels`, {
      method: "PUT",
      body: JSON.stringify(body),
    });
  },

  // Update the per-repo allowed content types gate. Body:
  // { allowed_content_types: ["document","url","doi"] | null }.
  // null = allow all (the default); an array restricts to the
  // listed kinds. Each value must be one of "document", "url", "doi".
  setRepositoryContentTypes(repoID, body) {
    return request(`/repositories/${repoID}/settings/content-types`, {
      method: "PUT",
      body: JSON.stringify(body),
    });
  },

  // Update the per-repo model override for a task kind. Body:
  // { task_kind, model_id }. Empty model_id clears the override
  // (revert to inheriting the global default).
  setRepositoryModel(repoID, body) {
    return request(`/repositories/${repoID}/settings/models`, {
      method: "PUT",
      body: JSON.stringify(body),
    });
  },

  listTasks(params = {}) {
    const qs = new URLSearchParams();
    if (params.state) qs.set("state", params.state);
    if (params.kind) qs.set("kind", params.kind);
    if (params.queue) qs.set("queue", params.queue);
    if (params.limit) qs.set("limit", params.limit);
    if (params.cursor) qs.set("cursor", params.cursor);
    const query = qs.toString();
    return request(`/tasks${query ? "?" + query : ""}`);
  },

  getTaskStats() {
    return request("/tasks/stats");
  },

  getTask(jobID) {
    return request(`/tasks/${jobID}`);
  },

  // rescueStuckJobs resets orphaned "running" jobs (whose owning
  // worker has a stale or missing heartbeat) back to "available"
  // so live workers re-process them. The recovery path for jobs
  // stuck in "running" after an API restart. Returns
  // { rescued, threshold }. Gated on the task:cancel permission.
  rescueStuckJobs(olderThan) {
    const qs = new URLSearchParams();
    if (olderThan) qs.set("older_than", olderThan);
    const query = qs.toString();
    return request(`/admin/tasks/rescue${query ? "?" + query : ""}`, { method: "POST" });
  },

  processSource(slug, sourceID) {
    return request(`/repositories/${slug}/sources/${sourceID}/process`, { method: "POST" });
  },

  // retrySource re-queues the retrieve_source pipeline for a row
  // whose fetch failed. The backend resets the row to 'pending'
  // and enqueues a fresh retrieve_source job (preserving the
  // stored DOI so the DOI path is retried when applicable).
  // Returns 202 with {job_id, source_id, status} — same shape as
  // /sources/retrieve.
  retrySource(slug, sourceID) {
    return request(`/repositories/${slug}/sources/${sourceID}/retry`, { method: "POST" });
  },

  listFacts(slug, sourceID, status = "", { q = "", limit = 100, offset = 0 } = {}) {
    const qs = new URLSearchParams({ limit, offset });
    if (status) qs.set("status", status);
    if (q) qs.set("q", q);
    return request(`/repositories/${slug}/sources/${sourceID}/facts?${qs.toString()}`);
  },

  listSourceReferences(slug, sourceID) {
    return request(`/repositories/${slug}/sources/${sourceID}/references`);
  },

  listRepoFacts(slug, status = "stable", sort = "", { q = "", limit = 100, offset = 0 } = {}) {
    const qs = new URLSearchParams({ status, limit, offset });
    if (sort) qs.set("sort", sort);
    if (q) qs.set("q", q);
    return request(`/repositories/${slug}/facts?${qs.toString()}`);
  },

  getFact(slug, factID) {
    return request(`/repositories/${slug}/facts/${factID}`);
  },

  // Investigations: a user-facing grouping of a subset of a repo's
  // sources (and transitively their facts). The repo scope comes
  // from the {slug} path segment; the X-Repository-ID header is
  // still sent for the system-side bookkeeping but the per-repo
  // middleware resolves the pool from the slug.
  listInvestigations(slug, { q = "", limit = 100, offset = 0 } = {}) {
    const qs = new URLSearchParams({ limit, offset });
    if (q) qs.set("q", q);
    return request(`/repositories/${slug}/investigations?${qs.toString()}`);
  },

  createInvestigation(slug, body) {
    return request(`/repositories/${slug}/investigations`, {
      method: "POST",
      body: JSON.stringify(body),
    });
  },

  getInvestigation(slug, invID) {
    return request(`/repositories/${slug}/investigations/${invID}`);
  },

  updateInvestigation(slug, invID, body) {
    return request(`/repositories/${slug}/investigations/${invID}`, {
      method: "PUT",
      body: JSON.stringify(body),
    });
  },

  deleteInvestigation(slug, invID) {
    return request(`/repositories/${slug}/investigations/${invID}`, {
      method: "DELETE",
    });
  },

  // --- Reports ---
  // A report is a user-authored markdown document that is
  // automatically annotated with supporting facts from the
  // repository. POST returns 202 + {report_id, job_id} (async
  // annotation); GET returns {report, annotations}.

  listReports(slug, { q = "", status = "", limit = 100, offset = 0 } = {}) {
    const qs = new URLSearchParams({ limit, offset });
    if (q) qs.set("q", q);
    if (status) qs.set("status", status);
    return request(`/repositories/${slug}/reports?${qs.toString()}`);
  },

  createReport(slug, body) {
    return request(`/repositories/${slug}/reports`, {
      method: "POST",
      body: JSON.stringify(body),
    });
  },

  async uploadReportFile(slug, file, title = "", topic = "", parentId = "") {
    const form = new FormData();
    form.append("file", file);
    if (title) form.append("title", title);
    if (topic) form.append("topic", topic);
    if (parentId) form.append("parent_id", parentId);
    const token = getToken();
    const headers = {};
    if (token) headers["Authorization"] = `Bearer ${token}`;
    // Do NOT set Content-Type — the browser sets it with the boundary.
    const res = await fetch(`${BASE}/repositories/${slug}/reports/upload`, {
      method: "POST",
      headers,
      body: form,
    });
    const data = await res.json();
    if (!res.ok) {
      throw new Error(data.error || "upload failed");
    }
    return data;
  },

  getReport(slug, reportID) {
    return request(`/repositories/${slug}/reports/${reportID}`);
  },

  updateReport(slug, reportID, body) {
    return request(`/repositories/${slug}/reports/${reportID}`, {
      method: "PUT",
      body: JSON.stringify(body),
    });
  },

  deleteReport(slug, reportID) {
    return request(`/repositories/${slug}/reports/${reportID}`, {
      method: "DELETE",
    });
  },

  annotateReport(slug, reportID) {
    return request(`/repositories/${slug}/reports/${reportID}/annotate`, {
      method: "POST",
    });
  },

  listReportAnnotations(slug, reportID) {
    return request(`/repositories/${slug}/reports/${reportID}/annotations`);
  },

  listInvestigationSources(slug, invID, { q = "", limit = 100, offset = 0 } = {}) {
    const qs = new URLSearchParams({ limit, offset });
    if (q) qs.set("q", q);
    return request(`/repositories/${slug}/investigations/${invID}/sources?${qs.toString()}`);
  },

  addInvestigationSource(slug, invID, sourceID) {
    return request(`/repositories/${slug}/investigations/${invID}/sources`, {
      method: "POST",
      body: JSON.stringify({ source_id: sourceID }),
    });
  },

  removeInvestigationSource(slug, invID, sourceID) {
    return request(`/repositories/${slug}/investigations/${invID}/sources/${sourceID}`, {
      method: "DELETE",
    });
  },

  listInvestigationFacts(
    slug,
    invID,
    status = "stable",
    sort = "",
    { q = "", limit = 100, offset = 0 } = {},
  ) {
    const qs = new URLSearchParams({ status, limit, offset });
    if (sort) qs.set("sort", sort);
    if (q) qs.set("q", q);
    return request(`/repositories/${slug}/investigations/${invID}/facts?${qs.toString()}`);
  },

  // Investigation-scoped concepts: only concepts derived from the
  // investigation's own sources' facts (via fact_concepts →
  // fact_sources → investigation_sources). A new investigation with
  // no processed sources returns an empty list, so concepts no
  // longer leak across investigations in the same repo.
  listInvestigationConcepts(slug, invID, { limit = 100, offset = 0 } = {}) {
    const qs = new URLSearchParams({ limit, offset });
    return request(`/repositories/${slug}/investigations/${invID}/concepts?${qs.toString()}`);
  },

  // Concepts. Concepts are produced by the extract_concepts worker
  // (chained after dedup, runs over stable facts). These endpoints
  // are the read surface. At the API level, per-context concept rows
  // (same canonical_name, different L3 context) are unified into one
  // group per canonical name, so:
  //   - listRepoConcepts / listInvestigationConcepts return one
  //     entry per canonical name with a `contexts` array.
  //   - getConcept returns the whole group (all contexts, each with
  //     its own aliases) by any concept_id in the group; the backend
  //     resolves the id to its canonical_name group.
  //   - listConceptFacts stays keyed on a per-context concept_id, so
  //     facts are compartmentalized per context.
  //   - listFactConcepts is the inverse view (per-context rows).
  //
  // Concepts are addressed by their UUID; the group's canonical_name
  // is the grouping key (lower(canonical_name)). The slug column was
  // removed in migration 0030.

  listRepoConcepts(slug, { q = "", limit = 100, offset = 0 } = {}) {
    const qs = new URLSearchParams({ limit, offset });
    if (q) qs.set("q", q);
    return request(`/repositories/${slug}/concepts?${qs.toString()}`);
  },

  // Primary detail endpoint: looks up the whole concept group by any
  // concept_id in the group (the backend resolves the id to its
  // canonical_name group).
  getConcept(slug, conceptID) {
    return request(`/repositories/${slug}/concepts/${conceptID}`);
  },

  // Concept relations: a first-class read surface showing which other
  // concepts share facts with this one. `shared_fact_count` is the
  // number of distinct facts linked to BOTH concepts (deduped per
  // fact, not per source). The list reads the concept_relations
  // materialized view (refreshed after each extract_concepts batch +
  // periodically); the details endpoint is a live per-context
  // breakdown for a specific pair. Both keyed by concept_id.
  listConceptRelations(slug, conceptID, { limit = 10, offset = 0 } = {}) {
    const qs = new URLSearchParams({ limit, offset });
    return request(`/repositories/${slug}/concepts/${conceptID}/relations?${qs.toString()}`);
  },

  getConceptRelationDetails(slug, conceptID, otherConceptID) {
    return request(`/repositories/${slug}/concepts/${conceptID}/relations/${otherConceptID}`);
  },

  listConceptFacts(slug, conceptID, { limit = 100, offset = 0, q = "" } = {}) {
    const qs = new URLSearchParams({ limit, offset });
    if (q) qs.set("q", q);
    return request(`/repositories/${slug}/concepts/${conceptID}/facts?${qs.toString()}`);
  },

  // Concept summaries: the summarize_concepts worker's read surface.
  // Returns one page-envelope row per summary slice (sequence_num
  // ascending). Each slice carries is_complete (FALSE = the open
  // accumulator still being regenerated; TRUE = a frozen BatchSize
  //   slice), fact_count, content (markdown with [text](<fact:fact_id>)
  //   citations), and covered_fact_ids.
  listConceptSummaries(slug, conceptID) {
    return request(`/repositories/${slug}/concepts/${conceptID}/summaries`);
  },

  // getConceptDefinition fetches the single synthesis ("definition")
  // for the concept_id's canonical-name group. The response is
  // { synthesis: {...}, images: [{id, image_url, text, fact_kind}] }
  // where images are the eager-loaded image facts the definition
  // embeds via ![alt](<fact:fact_id>) — the frontend resolves each
  // storage image_url to a blob URL before rendering. 404 when no
  // definition exists yet (the synthesize_concept worker hasn't run).
  getConceptDefinition(slug, conceptID) {
    return request(`/repositories/${slug}/concepts/${conceptID}/definition`);
  },

  listFactConcepts(slug, factID) {
    return request(`/repositories/${slug}/facts/${factID}/concepts`);
  },

  // Per-repo scoped task list. Filtered by the repo_id (and
  // optional source_id) metadata tag the backend enqueuer writes
  // on every job. Used by the Sources phase to show
  // ingestion/decomposition status per source.
  listRepoTasks(slug, { state = "", kind = "", source_id = "", limit = 100, cursor = "" } = {}) {
    const qs = new URLSearchParams({ limit });
    if (state) qs.set("state", state);
    if (kind) qs.set("kind", kind);
    if (source_id) qs.set("source_id", source_id);
    if (cursor) qs.set("cursor", cursor);
    return request(`/repositories/${slug}/tasks?${qs.toString()}`);
  },

  // AI usage dashboard. Each method accepts an optional params
  // object with from / to (RFC3339 timestamps) and repository_id
  // (UUID string); empty values are omitted from the query string.
  // The backend treats a missing param as "no filter".
  getAIUsageSummary(params = {}) {
    return request(`/ai/usage/summary${usageQS(params)}`);
  },

  getAIUsageByDay(params = {}) {
    return request(`/ai/usage/by-day${usageQS(params)}`);
  },

  getAIUsageByOperation(params = {}) {
    return request(`/ai/usage/by-operation${usageQS(params)}`);
  },

  getAIUsageByRepository(params = {}) {
    return request(`/ai/usage/by-repository${usageQS(params)}`);
  },

  // Remote registry: browse and pull sources from a configured
  // knowledge-registry instance. The /remote endpoints live under
  // /repositories/{slug}/remote so they are scoped to the current
  // repository and benefit from the per-repo middleware. Both
  // return 503 / "not configured" when the remote is not set up.

  listRemoteSources(slug, { q = "", limit = 20, offset = 0 } = {}) {
    const qs = new URLSearchParams({ limit, offset });
    if (q) qs.set("q", q);
    return request(`/repositories/${slug}/remote?${qs.toString()}`);
  },

  // getRemoteSource fetches the full SourcePackage for one
  // remote source (metadata + decomposition model list). The
  // backend proxies GET {registry}/api/v1/sources/{id} so the
  // frontend never talks to the registry directly (CORS / auth).
  getRemoteSource(slug, sourceID) {
    return request(`/repositories/${slug}/remote/${sourceID}`);
  },

  // getRemoteDecomposition fetches one model's facts + concepts
  // + links for a remote source. The backend proxies
  // GET {registry}/api/v1/sources/{id}/decompositions/{model}.
  // Used by the Remote Sources detail dialog when the user
  // expands a decomposition card to browse/troubleshoot the
  // contents without pulling the source locally.
  getRemoteDecomposition(slug, sourceID, modelID) {
    return request(
      `/repositories/${slug}/remote/${sourceID}/decompositions/${encodeURIComponent(modelID)}`,
    );
  },

  pullRemoteSource(slug, sourceID) {
    return request(`/repositories/${slug}/remote/${sourceID}/pull`, {
      method: "POST",
    });
  },

  // pullRemoteBatch enqueues a pull_remote_batch job that imports a
  // list of remote registry source IDs into the current repo. Used by
  // the "Pull page" and "Pull all results" buttons on the Remote
  // page. Returns { job_id, remote_source_count, status }.
  pullRemoteBatch(slug, remoteSourceIDs) {
    return request(`/repositories/${slug}/remote/pull-batch`, {
      method: "POST",
      body: JSON.stringify({ remote_source_ids: remoteSourceIDs }),
    });
  },

  getAIUsageBySource(params = {}) {
    return request(`/ai/usage/by-source${usageQS(params)}`);
  },

  // ── Promptsets ──────────────────────────────────────────────
  // listPromptsets returns the built-in + the caller's custom
  // promptsets (sysadmins see all). Used by the Promptsets page and
  // by the RepositorySettings promptset dropdown.
  listPromptsets() {
    return request(`/promptsets`);
  },

  // getPromptset returns a single promptset by hash (built-in or
  // custom the caller can see). 404 when unknown.
  getPromptset(hash) {
    return request(`/promptsets/${encodeURIComponent(hash)}`);
  },

  // createPromptset creates a custom promptset. The hash is
  // computed server-side from the 8 phase strings; the response
  // carries the authoritative hash.
  createPromptset(body) {
    return request(`/promptsets`, {
      method: "POST",
      body: JSON.stringify(body),
    });
  },

  // updatePromptset edits a custom promptset. Editing creates a
  // NEW row (new hash) since the hash is the identity; the
  // response is the new promptset. The old row stays so repos
  // pointing at it keep working.
  updatePromptset(hash, body) {
    return request(`/promptsets/${encodeURIComponent(hash)}`, {
      method: "PUT",
      body: JSON.stringify(body),
    });
  },

  // deletePromptset deletes a custom promptset the caller owns
  // (or any if sysadmin). The built-in cannot be deleted.
  deletePromptset(hash) {
    return request(`/promptsets/${encodeURIComponent(hash)}`, {
      method: "DELETE",
    });
  },

  // getRepositoryPromptset returns the repo's active + accepted
  // promptset hashes plus the resolved effective hash.
  getRepositoryPromptset(slug) {
    return request(`/repositories/${slug}/settings/promptset`);
  },

  // setRepositoryPromptset updates the repo's active + accepted
  // promptset hashes. Pass null active_hash to clear (inherit
  // global default); null accepted_hashes to clear the set.
  setRepositoryPromptset(slug, body) {
    return request(`/repositories/${slug}/settings/promptset`, {
      method: "PUT",
      body: JSON.stringify(body),
    });
  },

  // getRepositoryContributor returns the repo's contributor
  // identity ({ display_name: string|null, anonymous: bool }).
  // Used by the RepositorySettings ContributorPanel so the admin
  // can see whether pushes to the registry are attributed to a
  // display name or sent anonymously.
  getRepositoryContributor(slug) {
    return request(`/repositories/${slug}/settings/contributor`);
  },

  // setRepositoryContributor updates the repo's contributor
  // identity. Body: { display_name?: string|null, anonymous?: bool }.
  // When anonymous=true the server clears display_name; when
  // anonymous=false display_name must be a non-empty string.
  // Omit either field to leave it unchanged.
  setRepositoryContributor(slug, body) {
    return request(`/repositories/${slug}/settings/contributor`, {
      method: "PUT",
      body: JSON.stringify(body),
    });
  },
};

// usageQS builds the query string shared by the AI usage endpoints.
// bucket is only meaningful for by-day; it is ignored by the others.
function usageQS(params) {
  const qs = new URLSearchParams();
  if (params.from) qs.set("from", params.from);
  if (params.to) qs.set("to", params.to);
  if (params.repository_id) qs.set("repository_id", params.repository_id);
  if (params.bucket) qs.set("bucket", params.bucket);
  const s = qs.toString();
  return s ? "?" + s : "";
}
