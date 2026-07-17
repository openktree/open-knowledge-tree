import { Show, For } from "solid-js";
import Card from "../../components/Card";
import Button from "../../components/Button";
import EmptyState from "../../components/EmptyState";
import Pagination from "../../components/Pagination";
import SearchInput from "../../components/SearchInput";
import SourceFactRow from "./SourceFactRow";

// SourceFacts renders the facts extracted from a single source.
// Each fact carries a computed source_count so the user sees
// cross-confirmation: a fact extracted from this source may be
// confirmed by N-1 others. Each row is a link to the fact detail
// page (`/:slug/facts/:factID`) for full validation.
//
// Paginated and searchable. The resource (owned by
// SourceDetailPage) returns a pageEnvelope {data, total, limit,
// offset}; this component renders the rows and the paging bar.
export default function SourceFacts(props) {
  const rows = () => props.facts()?.data || [];
  const slug = () => props.slug;
  return (
    <Show when={rows().length > 0 || props.search()}>
      <div class="mt-6">
        <Card>
          <div class="flex items-center justify-between mb-3 gap-3 flex-wrap">
            <h2 class="text-lg font-semibold dark:text-white">
              Extracted facts ({props.total()})
            </h2>
            <div class="flex items-center gap-2">
              <SearchInput
                placeholder="Search fact text..."
                initial={props.search()}
                onSearch={props.onSearch}
              />
              <Button
                variant="secondary"
                onClick={props.onRefresh}
                class="text-xs px-2 py-1"
              >
                Refresh facts
              </Button>
            </div>
          </div>
          <Show
            when={rows().length > 0}
            fallback={
              <EmptyState
                title="No facts match your search."
                description="Try a different query, or clear the search box."
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
                Showing {props.offset() + 1}–{Math.min(props.offset() + props.limit, props.total())} of {props.total().toLocaleString()}
              </p>
              <div class="space-y-2 mt-3">
                <For each={rows()}>
                  {(fact) => <SourceFactRow fact={fact} slug={slug()} />}
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
        </Card>
      </div>
    </Show>
  );
}