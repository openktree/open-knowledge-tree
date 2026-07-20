import { createResource, createSignal, For, Show } from "solid-js";
import Badge from "../../components/Badge";
import Button from "../../components/Button";
import Card from "../../components/Card";
import EmptyState from "../../components/EmptyState";
import Loading from "../../components/Loading";
import Pagination from "../../components/Pagination";
import SearchInput from "../../components/SearchInput";
import { api } from "../../services/api";
import FactRow from "../Facts/FactRow";

export default function InvestigationFactsPhase(props) {
  const [statusFilter, setStatusFilter] = createSignal("stable");
  const [sort, setSort] = createSignal("");
  const [search, setSearch] = createSignal("");
  const [offset, setOffset] = createSignal(0);
  const [refreshKey, setRefreshKey] = createSignal(0);

  const [factData, { refetch }] = createResource(
    () => [props.slug, props.invID, statusFilter(), sort(), search(), offset(), refreshKey()],
    async ([s, id, status, sortBy, q, off]) => {
      if (!s || !id) return { data: [], total: 0, limit: 100, offset: 0 };
      try {
        return await api.listInvestigationFacts(s, id, status, sortBy, { q, offset: off });
      } catch (err) {
        props.onAlert?.({ variant: "error", message: err.message });
        return { data: [], total: 0, limit: 100, offset: 0 };
      }
    },
  );

  const facts = () => factData()?.data || [];
  const total = () => factData()?.total || 0;
  const limit = () => factData()?.limit || 100;

  const onSearch = (q) => {
    setSearch(q);
    setOffset(0);
  };

  return (
    <Card>
      <div class="flex items-center justify-between mb-4 gap-3 flex-wrap">
        <h2 class="text-lg font-semibold dark:text-white">Facts</h2>
        <div class="flex items-center gap-2 flex-wrap">
          <select
            class="text-sm border rounded px-2 py-1 dark:bg-gray-700 dark:border-gray-600 dark:text-gray-200"
            value={statusFilter()}
            onChange={(e) => {
              setStatusFilter(e.target.value);
              setOffset(0);
            }}
          >
            <option value="stable">Stable</option>
            <option value="new">New</option>
            <option value="to_delete">To delete</option>
            <option value="all">All</option>
          </select>
          <select
            class="text-sm border rounded px-2 py-1 dark:bg-gray-700 dark:border-gray-600 dark:text-gray-200"
            value={sort()}
            onChange={(e) => {
              setSort(e.target.value);
              setOffset(0);
            }}
          >
            <option value="">Newest first</option>
            <option value="source_count">Most confirmed</option>
          </select>
          <SearchInput placeholder="Search facts..." onSearch={onSearch} />
          <Button variant="secondary" onClick={refetch}>
            Refresh
          </Button>
        </div>
      </div>
      <Show when={!factData.loading} fallback={<Loading message="Loading facts..." />}>
        <Show
          when={facts().length > 0}
          fallback={
            <EmptyState
              title="No facts yet for this investigation's sources"
              description="Once the investigation's sources are processed, extracted facts will appear here."
            />
          }
        >
          <Show when={total() > limit()}>
            <Pagination
              total={total()}
              limit={limit()}
              offset={offset()}
              onOffsetChange={setOffset}
            />
            <p class="text-xs text-gray-500 dark:text-gray-400 mt-3">
              Showing {offset() + 1}–{Math.min(offset() + limit(), total())} of{" "}
              {total().toLocaleString()}
            </p>
          </Show>
          <div class="space-y-2 mt-3">
            <For each={facts()}>{(fact) => <FactRow fact={fact} slug={props.slug} />}</For>
          </div>
          <Show when={total() > limit()}>
            <Pagination
              total={total()}
              limit={limit()}
              offset={offset()}
              onOffsetChange={setOffset}
            />
          </Show>
        </Show>
      </Show>
    </Card>
  );
}
