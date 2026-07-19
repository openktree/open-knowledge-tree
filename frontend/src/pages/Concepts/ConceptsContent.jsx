import { For, Show } from "solid-js";
import Button from "../../components/Button";
import Card from "../../components/Card";
import EmptyState from "../../components/EmptyState";
import Pagination from "../../components/Pagination";
import SearchInput from "../../components/SearchInput";
import ConceptRow from "./ConceptRow";

// ConceptsContent is the main view for the repo-level Concepts
// page. Mirrors FactsContent: a Card with a count + Refresh button,
// paginated ConceptRow list, and the standard empty/loading/no-repo
// fallbacks. Concepts have no status lifecycle and no client-side
// sort toggle (the backend sorts by fact_count DESC, canonical_name
// ASC), so the toolbar is simpler than the Facts one.
export default function ConceptsContent(props) {
  const rows = () => props.concepts()?.data || [];
  return (
    <Card>
      <div class="flex items-center justify-between mb-3 gap-3 flex-wrap">
        <div class="flex items-center gap-3 flex-wrap">
          <h2 class="text-lg font-semibold dark:text-white">Concepts</h2>
          <SearchInput
            placeholder="Search concepts..."
            initial={props.search()}
            onSearch={props.onSearch}
          />
        </div>
        <div class="flex items-center gap-3">
          <span class="text-xs text-gray-500 dark:text-gray-400" data-testid="concepts-total">
            {props.total().toLocaleString()} concept{props.total() === 1 ? "" : "s"}
          </span>
          <Button variant="secondary" onClick={props.onRefresh} class="text-xs px-2 py-1">
            Refresh
          </Button>
        </div>
      </div>
      <p class="text-sm text-gray-500 dark:text-gray-400 mb-4">
        Concepts extracted from stable facts. Each concept links to the facts that mention it.
      </p>

      <Show
        when={props.hasRepo()}
        fallback={
          <EmptyState
            title="Select a repository to view its concepts."
            description="Use the repository dropdown in the top bar."
          />
        }
      >
        <Show
          when={rows().length > 0}
          fallback={
            <EmptyState
              title={props.search() ? "No concepts match your search." : "No concepts yet."}
              description={
                props.search()
                  ? "Try a different query, or clear the search box."
                  : "Concepts are extracted automatically once facts are processed and deduplicated. Process sources to generate facts, then wait for the concept-extraction pipeline to run."
              }
            />
          }
        >
          <>
            <Pagination
              total={props.total}
              limit={props.limit}
              offset={props.offset()}
              onOffsetChange={props.onOffsetChange}
            />
            <p class="text-xs text-gray-500 dark:text-gray-400 mt-3">
              Showing {props.offset() + 1}–{Math.min(props.offset() + props.limit, props.total())}{" "}
              of {props.total().toLocaleString()}
            </p>
            <div class="space-y-2 mt-3">
              <For each={rows()}>
                {(concept) => <ConceptRow concept={concept} slug={props.slug()} />}
              </For>
            </div>
            <Pagination
              total={props.total}
              limit={props.limit}
              offset={props.offset()}
              onOffsetChange={props.onOffsetChange}
            />
          </>
        </Show>
      </Show>
    </Card>
  );
}
