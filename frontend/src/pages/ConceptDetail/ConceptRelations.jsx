import { For, Show, createResource, createSignal } from "solid-js";
import { A } from "@solidjs/router";
import Card from "../../components/Card";
import Badge from "../../components/Badge";
import EmptyState from "../../components/EmptyState";
import Pagination from "../../components/Pagination";
import { api } from "../../services/api";

const PAGE_SIZE = 10;

// ConceptRelations renders the "Related concepts" card on the concept
// detail page. A relation between two concepts is the set of facts
// linked to BOTH; shared_fact_count is the distinct count of those
// shared facts (deduped per fact, not per source). The list reads the
// concept_relations materialized view (refreshed after each
// extract_concepts batch + periodically), so it is parallel-safe and
// shows the top-N relations by shared_fact_count DESC.
//
// Group-level (not per-context-tab): relations are between concept
// GROUPS (unified by canonical name), so this card sits below the
// ContextPanel and does not re-fetch when the active context changes.
// Pagination controls (prev/next + page numbers) replace the list per
// page; default page size 10. Each row links to the related concept's
// detail page (by its representative concept_id) so the user can
// navigate the relation graph.
export default function ConceptRelations(props) {
  const slug = () => props.slug;
  const conceptID = () => props.conceptID;

  const [offset, setOffset] = createSignal(0);
  const [refreshKey, setRefreshKey] = createSignal(0);
  const [relData, { refetch }] = createResource(
    () => ({ slug: slug(), conceptID: conceptID(), offset: offset(), key: refreshKey() }),
    async ({ slug, conceptID, offset }) => {
      if (!slug || !conceptID) return { data: [], total: 0, limit: PAGE_SIZE, offset: 0 };
      try {
        return await api.listConceptRelations(slug, conceptID, { limit: PAGE_SIZE, offset });
      } catch {
        return { data: [], total: 0, limit: PAGE_SIZE, offset: 0 };
      }
    }
  );

  const relations = () => relData()?.data || [];
  const total = () => relData()?.total || 0;
  const limit = () => relData()?.limit || PAGE_SIZE;

  const handleOffset = (off) => {
    setOffset(off);
    setRefreshKey((k) => k + 1);
  };

  return (
    <Card class="xl:max-h-[calc(100vh-8rem)] xl:flex xl:flex-col">
      <div class="flex items-center justify-between mb-4 gap-3 flex-wrap">
        <div class="flex items-center gap-2 flex-wrap">
          <h2 class="text-lg font-semibold dark:text-white">Relations</h2>
          <Badge variant="gray">{total().toLocaleString()} related concept{total() === 1 ? "" : "s"}</Badge>
        </div>
        <button
          type="button"
          onClick={refetch}
          class="text-xs px-2 py-1 rounded border border-gray-300 dark:border-gray-600 text-gray-700 dark:text-gray-300 hover:bg-gray-50 dark:hover:bg-gray-800"
        >
          Refresh
        </button>
      </div>

      <Show
        when={relations().length > 0}
        fallback={
          <EmptyState
            title="No relations yet."
            description="Once this concept shares facts with other concepts, related concepts will appear here ranked by shared facts."
          />
        }
      >
        <div class="space-y-2 xl:flex-1 xl:overflow-y-auto">
          <For each={relations()}>
            {(rel) => (
              <A
                href={`/${slug()}/concepts/${rel.concept_id}`}
                class="flex items-center justify-between gap-2 p-3 border rounded dark:border-gray-700 hover:bg-gray-50 dark:hover:bg-gray-800/50 transition-colors text-sm"
                title={`View ${rel.canonical_name} detail`}
              >
                <span class="font-medium dark:text-white truncate">{rel.canonical_name}</span>
                <Badge variant="blue">{rel.shared_fact_count.toLocaleString()} shared</Badge>
              </A>
            )}
          </For>
        </div>

        <Show when={total() > limit()}>
          <div class="pt-3 border-t border-gray-200 dark:border-gray-700 mt-3">
            <Pagination total={total()} limit={limit()} offset={offset()} onOffsetChange={handleOffset} />
          </div>
        </Show>
      </Show>
    </Card>
  );
}