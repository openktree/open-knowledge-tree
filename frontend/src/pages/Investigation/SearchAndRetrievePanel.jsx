import { createResource, createSignal, Show, For } from "solid-js";
import { api } from "../../services/api";
import { useRepository } from "../../store/repository";
import Alert from "../../components/Alert";
import Button from "../../components/Button";
import CollapsibleSection from "../../components/CollapsibleSection";
import FormField from "../../components/FormField";
import SearchResult from "../../components/SearchResult";
import { useRetrieveAndLink } from "./useRetrieveAndLink";

export default function SearchAndRetrievePanel(props) {
  const repo = useRepository();
  const [query, setQuery] = createSignal("");
  const [provider, setProvider] = createSignal("");
  const [results, setResults] = createSignal(null);
  const [total, setTotal] = createSignal(0);
  const [cursor, setCursor] = createSignal("");
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal("");
  const [info, setInfo] = createSignal("");
  const [progress, setProgressMap] = createSignal({});

  const [providers] = createResource(() => api.listProviders().catch(() => ({ search: [] })));
  const searchProviders = () =>
    (providers()?.search || []).filter((p) => p.enabled_for_repo !== false);
  const repoID = () => repo.currentRepo()?.id || "";

  // The retrieve-and-link hook owns the fetch → poll → auto-link
  // pipeline. It writes progress into our local signal via the
  // setter so the UI can show per-result state.
  const hook = useRetrieveAndLink({
    slug: props.slug,
    invID: props.invID,
    onLinked: () => {
      setInfo("Source fetched and added to investigation");
      props.onSourceLinked?.();
    },
  });
  hook.setProgressSetter((key, value) => setProgressMap((p) => ({ ...p, [key]: value })));

  const runSearch = async (cursorVal) => {
    const q = query().trim();
    if (!q) return;
    const pid = provider() || searchProviders()[0]?.id;
    if (!pid) {
      setError("No search provider configured");
      return;
    }
    setBusy(true);
    setError("");
    if (!cursorVal) {
      setResults(null);
      setTotal(0);
      setCursor("");
      setProgressMap({});
    }
    try {
      const resp = await api.testSearch(pid, q, { repository_id: repoID(), cursor: cursorVal });
      if (cursorVal) {
        setResults((prev) => [...(prev || []), ...(resp.results || [])]);
        setTotal((prev) => prev || resp.total || 0);
      } else {
        setResults(resp.results || []);
        setTotal(resp.total || 0);
      }
      setCursor(resp.next_cursor || "");
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy(false);
    }
  };

  const onSearch = (e) => {
    e.preventDefault();
    runSearch("");
  };

  const onLoadMore = () => runSearch(cursor());

  const handleFetch = (result, process = false) => {
    setInfo("");
    hook.enqueue(result, repoID(), process);
  };

  return (
    <CollapsibleSection
      title="Search & add sources"
      subtitle="Search a provider, then fetch & process. Fetched sources are auto-added to this investigation."
    >
      <form onSubmit={onSearch} class="flex gap-2 mb-4 flex-wrap">
        <Show when={searchProviders().length > 0}>
          <FormField type="select" value={provider()} onChange={setProvider} inputClass="min-w-0">
            <For each={searchProviders()}>
              {(p) => <option value={p.id}>{p.name}</option>}
            </For>
          </FormField>
        </Show>
        <FormField value={query()} onChange={setQuery} placeholder="Enter search query..." class="flex-1" />
        <Button type="submit" loading={busy()} loadingText="Searching...">Search</Button>
      </form>
      <Alert variant="error" message={error()} onDismiss={() => setError("")} />
      <Show when={info()}>
        <Alert variant="success" message={info()} onDismiss={() => setInfo("")} />
      </Show>
      <Show when={results()}>
        <Show when={total() > 0}>
          <p class="text-xs text-gray-500 dark:text-gray-400 mb-2">{total()} total results</p>
        </Show>
        <div class="space-y-3">
          <For each={results()}>
            {(r) => (
              <SearchResult
                result={r}
                progress={progress()[r.url]}
                onFetch={handleFetch}
                fetchLabel="Fetch & Add"
                reFetchLabel="Re-fetch"
              />
            )}
          </For>
        </div>
        <Show when={cursor()}>
          <div class="mt-4">
            <Button variant="secondary" onClick={onLoadMore} loading={busy()} loadingText="Loading...">
              Load more
            </Button>
          </div>
        </Show>
      </Show>
      <Show when={!results() && !error() && !busy()}>
        <p class="text-sm text-gray-400 dark:text-gray-500">
          Enter a query and click Search to find sources to add.
        </p>
      </Show>
    </CollapsibleSection>
  );
}