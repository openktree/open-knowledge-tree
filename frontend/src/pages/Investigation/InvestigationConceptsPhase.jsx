import { createResource, createSignal, For, Show } from "solid-js";
import Badge from "../../components/Badge";
import Button from "../../components/Button";
import Card from "../../components/Card";
import EmptyState from "../../components/EmptyState";
import Loading from "../../components/Loading";
import Pagination from "../../components/Pagination";
import { api } from "../../services/api";
import ConceptRow from "../Concepts/ConceptRow";

const PAGE_SIZE = 100;

// InvestigationConceptsPhase is the "Concepts" tab body inside an
// investigation. It calls the investigation-scoped concepts endpoint,
// which only returns concepts derived from facts that came from this
// investigation's own sources (via fact_concepts → fact_sources →
// investigation_sources). A new investigation with no processed
// sources returns an empty list, so concepts no longer leak across
// investigations in the same repo. The concepts are produced
// automatically by the extract_concepts worker chained after dedup,
// so this tab is read-only: it surfaces what the pipeline has
// generated from the investigation's sources' facts.
export default function InvestigationConceptsPhase(props) {
  const [offset, setOffset] = createSignal(0);
  const [refreshKey, setRefreshKey] = createSignal(0);

  const [conceptData, { refetch }] = createResource(
    () => [props.slug, props.invID, offset(), refreshKey()],
    async ([s, id, off]) => {
      if (!s || !id) return { data: [], total: 0, limit: PAGE_SIZE, offset: 0 };
      try {
        return await api.listInvestigationConcepts(s, id, { limit: PAGE_SIZE, offset: off });
      } catch (err) {
        props.onAlert?.({ variant: "error", message: err.message });
        return { data: [], total: 0, limit: PAGE_SIZE, offset: 0 };
      }
    },
  );

  const concepts = () => conceptData()?.data || [];
  const total = () => conceptData()?.total || 0;
  const limit = () => conceptData()?.limit || PAGE_SIZE;

  return (
    <Card>
      <div class="flex items-center justify-between mb-4 gap-3 flex-wrap">
        <h2 class="text-lg font-semibold dark:text-white">Concepts</h2>
        <Button variant="secondary" onClick={refetch}>
          Refresh
        </Button>
      </div>
      <p class="text-sm text-gray-500 dark:text-gray-400 mb-4">
        Concepts extracted from this investigation's stable facts. Each concept links to the facts
        that mention it.
      </p>
      <Show when={!conceptData.loading} fallback={<Loading message="Loading concepts..." />}>
        <Show
          when={concepts().length > 0}
          fallback={
            <EmptyState
              title="No concepts yet for this investigation"
              description="Concepts are extracted automatically once this investigation's sources are processed and their facts are deduplicated. Process the investigation's sources to generate facts, then wait for the concept-extraction pipeline to run."
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
            <For each={concepts()}>
              {(concept) => <ConceptRow concept={concept} slug={props.slug} />}
            </For>
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
