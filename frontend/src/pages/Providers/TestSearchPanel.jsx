import { createSignal, Show, For } from "solid-js";
import { api } from "../../services/api";
import { useRepository } from "../../store/repository";
import Alert from "../../components/Alert";
import Button from "../../components/Button";
import Card from "../../components/Card";
import FormField from "../../components/FormField";
import SearchResult from "../../components/SearchResult";

export default function TestSearchPanel(props) {
  const repo = useRepository();
  const [searchQuery, setSearchQuery] = createSignal("");
  const [searchResults, setSearchResults] = createSignal(null);
  const [searchTotal, setSearchTotal] = createSignal(0);
  const [nextCursor, setNextCursor] = createSignal("");
  const [searchError, setSearchError] = createSignal("");
  const [searching, setSearching] = createSignal(false);
  const [loadingMore, setLoadingMore] = createSignal(false);
  const [selectedSearchProvider, setSelectedSearchProvider] = createSignal("");
  const [classifyResults, setClassifyResults] = createSignal({});
  const [enqueueResults, setEnqueueResults] = createSignal({});

  const searchProviders = () => (props.providers().search || []).filter(
    (p) => p.enabled_for_repo !== false
  );

  // allAlreadyAdded is true when every loaded result is tagged
  // already_exists. The UI uses it to render a prominent
  // "everything here is already in your library — Load more"
  // button so the user can keep paging without re-clicking.
  const allAlreadyAdded = () => {
    const rs = searchResults();
    if (!rs || rs.length === 0) return false;
    return rs.every((r) => r.already_exists);
  };

  const runSearch = async (providerId, query, cursor) => {
    const repoID = repo.currentRepo()?.id || "";
    return api.testSearch(providerId, query, { repository_id: repoID, cursor });
  };

  const handleSearch = async (e) => {
    e.preventDefault();
    if (!searchQuery().trim()) return;
    const providerId = selectedSearchProvider() || searchProviders()[0]?.id;
    if (!providerId) {
      setSearchError("No search provider available");
      return;
    }
    setSearching(true);
    setSearchError("");
    setSearchResults(null);
    setSearchTotal(0);
    setNextCursor("");
    setClassifyResults({});
    setEnqueueResults({});
    try {
      const resp = await runSearch(providerId, searchQuery().trim(), "");
      setSearchResults(resp.results || []);
      setSearchTotal(resp.total || 0);
      setNextCursor(resp.next_cursor || "");
    } catch (err) {
      setSearchError(err.message);
    } finally {
      setSearching(false);
    }
  };

  const handleLoadMore = async () => {
    if (!nextCursor()) return;
    const providerId = selectedSearchProvider() || searchProviders()[0]?.id;
    if (!providerId) return;
    setLoadingMore(true);
    setSearchError("");
    try {
      const resp = await runSearch(providerId, searchQuery().trim(), nextCursor());
      setSearchResults((prev) => [...(prev || []), ...(resp.results || [])]);
      // OpenAlex surfaces a stable total; Serper reports 0 (unknown).
      // Keep the first non-zero total we see so the UI can show it.
      setSearchTotal((prev) => prev || resp.total || 0);
      setNextCursor(resp.next_cursor || "");
    } catch (err) {
      setSearchError(err.message);
    } finally {
      setLoadingMore(false);
    }
  };

  const handleClassify = async (url) => {
    setClassifyResults((prev) => ({ ...prev, [url]: { loading: true } }));
    try {
      const result = await api.classifyResource(url);
      setClassifyResults((prev) => ({ ...prev, [url]: result }));
    } catch (err) {
      setClassifyResults((prev) => ({ ...prev, [url]: { error: err.message } }));
    }
  };

  const handleFetch = async (result, process = false) => {
    const url = result.url;
    const doi = result.doi;
    setEnqueueResults((prev) => ({ ...prev, [url]: { loading: true } }));
    try {
      const result2 = await api.retrieveSource(url, repo.currentRepo()?.id || "", process, doi);
      setEnqueueResults((prev) => ({
        ...prev,
        [url]: {
          loading: false,
          jobId: result2.job_id,
          classifiedAs: result2.classified_as,
          value: result2.value,
          process,
        },
      }));
    } catch (err) {
      setEnqueueResults((prev) => ({ ...prev, [url]: { loading: false, error: err.message } }));
    }
  };

  return (
    <Card class="mb-6">
      <h2 class="text-lg font-semibold mb-1 dark:text-white">Test Search</h2>
      <p class="text-sm text-gray-500 dark:text-gray-400 mb-4">
        Run a search query through a configured search provider. Use the
        Fetch button on a result to enqueue a retrieve_source job, or
        Fetch &amp; Process to also chain the source_decomposition job once
        the fetch lands. Results already in the current repository are
        marked with an "Already added" badge.
      </p>

      <form onSubmit={handleSearch} class="flex gap-2 mb-4">
        <FormField
          type="select"
          value={selectedSearchProvider()}
          onChange={setSelectedSearchProvider}
          inputClass="min-w-0"
        >
          <For each={searchProviders()}>
            {(p) => <option value={p.id}>{p.name}</option>}
          </For>
        </FormField>
        <FormField
          value={searchQuery()}
          onChange={setSearchQuery}
          placeholder="Enter search query..."
          class="flex-1"
        />
        <Button type="submit" loading={searching()} loadingText="Searching...">
          Search
        </Button>
      </form>

      <Alert variant="error" message={searchError()} onDismiss={() => setSearchError("")} />

      <Show when={searchResults()}>
        <Show when={searchTotal() > 0}>
          <p class="text-xs text-gray-500 dark:text-gray-400 mb-2">
            {searchTotal()} total results
          </p>
        </Show>
        <div class="space-y-3">
          <For each={searchResults()}>
            {(result) => (
              <SearchResult
                result={result}
                classify={() => classifyResults()[result.url]}
                enqueue={() => enqueueResults()[result.url]}
                onClassify={handleClassify}
                onFetch={handleFetch}
              />
            )}
          </For>
        </div>
        <Show when={nextCursor()}>
          <div class="mt-4">
            <Show
              when={allAlreadyAdded()}
              fallback={
                <Button variant="secondary" onClick={handleLoadMore} loading={loadingMore()} loadingText="Loading...">
                  Load more
                </Button>
              }
            >
              <div class="p-3 rounded border border-amber-300 dark:border-amber-700 bg-amber-50 dark:bg-amber-900/30">
                <p class="text-sm text-amber-800 dark:text-amber-300 mb-2">
                  All results on this page are already in your library.
                </p>
                <Button variant="primary" onClick={handleLoadMore} loading={loadingMore()} loadingText="Loading...">
                  Load more
                </Button>
              </div>
            </Show>
          </div>
        </Show>
      </Show>

      <Show when={!searchResults() && !searchError() && !searching()}>
        <p class="text-sm text-gray-400 dark:text-gray-500">
          Enter a query and click Search to test the provider.
        </p>
      </Show>
    </Card>
  );
}