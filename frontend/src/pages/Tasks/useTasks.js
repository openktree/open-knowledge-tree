import { createEffect, createSignal, onMount } from "solid-js";
import { api } from "../../services/api";

const PAGE_SIZE = 50;

// useTasks owns the tasks-page state: the filter signals, the
// accumulated jobs list, the River cursor, the load/Load-more
// fetch actions, and the system-wide task stats. Kept in a hook
// so index.jsx stays a thin view (the page-size policy caps a
// page folder's index.jsx at 100 lines when it has an internal
// subcomponent — the default export itself — and 150 otherwise).
//
// `stats` is fetched once on mount (system-wide overview,
// independent of filters). `jobs` is fetched on mount and again
// whenever a filter signal flips.
//
// Returns an object with the signals + actions the view binds to.
// The signals are Solid accessors; the view reads them in JSX and
// calls the actions on user input.
//
// The optional repoSlug argument switches the hook from the
// system-wide endpoints (api.listTasks / api.getTaskStats) to the
// per-repo scoped ones (api.listRepoTasks / api.getRepoTaskStats).
// The RepoTasks page passes the current repo's slug; the system
// Tasks page leaves it undefined. Rescue is always system-side
// (the repo page passes canRescue=false to the stats card so the
// rescue action is never invoked from the repo page).
export function useTasks(repoSlug) {
  const [state, setState] = createSignal("");
  const [kind, setKind] = createSignal("");
  const [queue, setQueue] = createSignal("");
  const [alert, setAlert] = createSignal(null);
  const [jobs, setJobs] = createSignal([]);
  const [cursor, setCursor] = createSignal(null);
  const [hasMore, setHasMore] = createSignal(false);
  const [loading, setLoading] = createSignal(false);
  const [loadingMore, setLoadingMore] = createSignal(false);
  const [stats, setStats] = createSignal(null);
  const [statsLoading, setStatsLoading] = createSignal(true);
  const [rescuing, setRescuing] = createSignal(false);
  const [reextracting, setReextracting] = createSignal(false);
  const [recomputing, setRecomputing] = createSignal(false);

  // fetchPage applies current filters + optional cursor. append
  // concatenates (Load more); otherwise replaces (initial load +
  // filter change).
  async function fetchPage({ cursor: pageCursor = null, append = false } = {}) {
    if (append) {
      setLoadingMore(true);
    } else {
      setLoading(true);
      setAlert(null);
    }
    try {
      const params = { limit: PAGE_SIZE };
      if (state()) params.state = state();
      if (kind()) params.kind = kind();
      if (queue()) params.queue = queue();
      if (pageCursor) params.cursor = pageCursor;
      const data = repoSlug
        ? await api.listRepoTasks(repoSlug, params)
        : await api.listTasks(params);
      const next = data.jobs || [];
      setJobs((cur) => (append ? [...cur, ...next] : next));
      setHasMore(!!data.has_more);
      setCursor(data.next_cursor || null);
    } catch (err) {
      setAlert({ variant: "error", message: err.message });
      if (!append) {
        setJobs([]);
        setHasMore(false);
        setCursor(null);
      }
    } finally {
      if (append) setLoadingMore(false);
      else setLoading(false);
    }
  }

  function reload() {
    fetchPage({ append: false });
  }

  function loadMore() {
    if (!hasMore() || loadingMore() || !cursor()) return;
    fetchPage({ cursor: cursor(), append: true });
  }

  // fetchStats loads the system-wide task stats once on mount.
  // It is independent of the job-list filters because the stats
  // card shows the overall backlog, not a filtered subset.
  async function fetchStats() {
    setStatsLoading(true);
    try {
      const data = repoSlug ? await api.getRepoTaskStats(repoSlug) : await api.getTaskStats();
      setStats(data);
    } catch {
      setStats(null);
    } finally {
      setStatsLoading(false);
    }
  }

  onMount(fetchStats);

  // reloadStats re-fetches the system-wide task stats on demand
  // (the refresh button on the stats card). Independent of the
  // job-list filters/reload so the card reflects the live backlog
  // without disturbing the filtered table below.
  const reloadStats = () => fetchStats();

  // rescueStuckJobs calls the admin endpoint that resets orphaned
  // "running" jobs (owned by dead workers) back to "available". On
  // success it refreshes the stats so the UI reflects the change.
  // Returns the API result so the caller can surface a confirmation.
  async function rescueStuckJobs(olderThan) {
    setRescuing(true);
    setAlert(null);
    try {
      const result = await api.rescueStuckJobs(olderThan);
      await fetchStats();
      return result;
    } catch (err) {
      setAlert({ variant: "error", message: err.message });
      return null;
    } finally {
      setRescuing(false);
    }
  }

  // reextractConcepts calls the admin endpoint that clears
  // retryable fact_concept_skips + unresolved fact_candidates
  // for a repo and enqueues a repo-wide extract_concepts job.
  // On-demand recovery from the historical permanent-skip bug.
  // Returns the API result so the caller can surface a toast.
  async function reextractConcepts(repoID, maxAttempts) {
    setReextracting(true);
    setAlert(null);
    try {
      const result = await api.reextractRepoConcepts(repoID, maxAttempts);
      await fetchStats();
      return result;
    } catch (err) {
      setAlert({ variant: "error", message: err.message });
      return null;
    } finally {
      setReextracting(false);
    }
  }

  // recomputeConceptGroups calls the admin endpoint that enqueues a
  // full concept_groups summary recompute for a repo. The repair
  // path; the ingest workers keep the summary live incrementally
  // otherwise. Returns the API result so the caller can surface a
  // toast with the enqueued job id.
  async function recomputeConceptGroups(repoID) {
    setRecomputing(true);
    setAlert(null);
    try {
      const result = await api.recomputeRepoConceptGroups(repoID);
      await fetchStats();
      return result;
    } catch (err) {
      setAlert({ variant: "error", message: err.message });
      return null;
    } finally {
      setRecomputing(false);
    }
  }

  // Reload whenever a filter signal flips. The lastFilters guard
  // prevents a loop on setJobs (createEffect re-runs whenever a
  // signal it read changes; we only fetch when the filter combo
  // actually changed).
  let lastFilters = "";
  createEffect(() => {
    const k = `${state()}|${kind()}|${queue()}`;
    if (k !== lastFilters) {
      lastFilters = k;
      reload();
    }
  });

  return {
    state,
    setState,
    kind,
    setKind,
    queue,
    setQueue,
    alert,
    setAlert,
    jobs,
    hasMore,
    loading,
    loadingMore,
    stats,
    statsLoading,
    rescuing,
    reextracting,
    recomputing,
    reload,
    loadMore,
    reloadStats,
    rescueStuckJobs,
    reextractConcepts,
    recomputeConceptGroups,
  };
}
