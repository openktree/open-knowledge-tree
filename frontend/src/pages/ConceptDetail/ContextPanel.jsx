import { For, Show, createResource, createSignal } from "solid-js";
import Card from "../../components/Card";
import Button from "../../components/Button";
import Badge from "../../components/Badge";
import EmptyState from "../../components/EmptyState";
import Pagination from "../../components/Pagination";
import SearchInput from "../../components/SearchInput";
import FactRow from "../Facts/FactRow";
import FactSourceBadge from "./FactSourceBadge";
import SummaryPanel from "./SummaryPanel";
import { api } from "../../services/api";
import { PAGE_SIZE } from "./constants";

// ContextPanel renders the selected context's slice of a concept
// group: the context's description and the paginated facts linked
// to that context's concept_id (the "query DNA → facts" view,
// scoped to THIS context only). Metadata and aliases for the
// active context are rendered in the ConceptHeader card; this
// panel focuses on summaries + facts.
//
// The SearchInput drives a server-side `q` filter against
// facts.search_tsv (websearch_to_tsquery), so the search reaches
// facts beyond the current page — important for large concepts
// whose facts span many pages. Changing the query resets the
// offset to 0 and re-fetches.
export default function ContextPanel(props) {
  const slug = () => props.slug;
  const context = () => props.context;
  const conceptID = () => context()?.concept_id || "";

  const [offset, setOffset] = createSignal(0);
  const [search, setSearch] = createSignal("");
  const [refreshKey, setRefreshKey] = createSignal(0);
  const [factData, { refetch }] = createResource(
    () => ({ slug: slug(), conceptID: conceptID(), offset: offset(), q: search(), key: refreshKey() }),
    async ({ slug, conceptID, offset, q }) => {
      if (!slug || !conceptID) return { data: [], total: 0, limit: PAGE_SIZE, offset: 0 };
      try {
        return await api.listConceptFacts(slug, conceptID, { limit: PAGE_SIZE, offset, q });
      } catch {
        return { data: [], total: 0, limit: PAGE_SIZE, offset: 0 };
      }
    }
  );

  const facts = () => factData()?.data || [];
  const total = () => factData()?.total || 0;
  const limit = () => factData()?.limit || PAGE_SIZE;

  const handleOffset = (off) => {
    setOffset(off);
    setRefreshKey((k) => k + 1);
  };

  const handleSearch = (q) => {
    setSearch(q);
    setOffset(0);
    setRefreshKey((k) => k + 1);
  };

  return (
    <div class="space-y-6">
      <SummaryPanel slug={slug()} conceptID={conceptID()} />

      <Card>
        <div class="flex items-center justify-between mb-4 gap-3 flex-wrap">
          <div class="flex items-center gap-2 flex-wrap">
            <h2 class="text-lg font-semibold dark:text-white">{context()?.context || "Context"}</h2>
            <Badge variant="gray">{total().toLocaleString()} fact{total() === 1 ? "" : "s"}</Badge>
          </div>
          <div class="flex items-center gap-2 flex-wrap">
            <SearchInput
              placeholder="Search fact text..."
              initial={search()}
              onSearch={handleSearch}
            />
            <Button variant="secondary" onClick={refetch} class="text-xs px-2 py-1">
              Refresh
            </Button>
          </div>
        </div>

        <Show when={context()?.description}>
          <div class="text-xs text-gray-500 dark:text-gray-400 mb-4">{context()?.description}</div>
        </Show>

        <Show
          when={facts().length > 0}
          fallback={
            <EmptyState
              title={search() ? "No facts match your search." : "No facts linked to this context yet."}
              description={search() ? "Try a different query, or clear the search box." : "Once facts are processed and concepts extracted, the facts mentioning this concept under this context will appear here."}
            />
          }
        >
          <Show when={total() > limit()}>
            <Pagination total={total()} limit={limit()} offset={offset()} onOffsetChange={handleOffset} />
            <p class="text-xs text-gray-500 dark:text-gray-400 mt-3">
              Showing {offset() + 1}–{Math.min(offset() + limit(), total())} of {total().toLocaleString()}
            </p>
          </Show>
          <div class="space-y-2 mt-3">
            <For each={facts()}>
              {(fact) => (
                <FactRow fact={fact} slug={slug()} extra={<FactSourceBadge slug={slug()} factID={fact.id} />} />
              )}
            </For>
          </div>
          <Show when={total() > limit()}>
            <Pagination total={total()} limit={limit()} offset={offset()} onOffsetChange={handleOffset} />
          </Show>
        </Show>
      </Card>
    </div>
  );
}