import { For, Show } from "solid-js";
import Button from "../../components/Button";
import Card from "../../components/Card";
import EmptyState from "../../components/EmptyState";
import Pagination from "../../components/Pagination";
import SearchInput from "../../components/SearchInput";
import FactRow from "./FactRow";

export default function FactsContent(props) {
  const rows = () => props.facts()?.data || [];
  return (
    <Card>
      <div class="flex items-center justify-between mb-3 gap-3 flex-wrap">
        <div class="flex items-center gap-3 flex-wrap">
          <h2 class="text-lg font-semibold dark:text-white">Extracted facts</h2>
          <select
            value={props.statusFilter()}
            onChange={(e) => props.onStatusChange(e.target.value)}
            class="text-xs border rounded dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-700 dark:text-gray-200 px-2 py-1"
          >
            <For each={props.statusOptions}>
              {(opt) => <option value={opt.value}>{opt.label}</option>}
            </For>
          </select>
          <select
            value={props.sort()}
            onChange={(e) => props.onSortChange(e.target.value)}
            class="text-xs border rounded dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-700 dark:text-gray-200 px-2 py-1"
            title="Sort facts by newest first or by most confirmed (source count)"
          >
            <For each={props.sortOptions}>
              {(opt) => <option value={opt.value}>{opt.label}</option>}
            </For>
          </select>
          <SearchInput
            placeholder="Search fact text..."
            initial={props.search()}
            onSearch={props.onSearch}
          />
        </div>
        <div class="flex items-center gap-3">
          <span class="text-xs text-gray-500 dark:text-gray-400" data-testid="facts-total">
            {props.total().toLocaleString()} fact{props.total() === 1 ? "" : "s"}
          </span>
          <Button variant="secondary" onClick={props.onRefresh} class="text-xs px-2 py-1">
            Refresh
          </Button>
        </div>
      </div>
      <p class="text-sm text-gray-500 dark:text-gray-400 mb-4">
        Atomic factual claims extracted from processed sources in this repository.
      </p>

      <Show
        when={props.hasRepo()}
        fallback={
          <EmptyState
            title="Select a repository to view its facts."
            description="Use the repository dropdown in the top bar."
          />
        }
      >
        <Show
          when={rows().length > 0}
          fallback={
            <EmptyState
              title={props.search() ? "No facts match your search." : "No facts yet."}
              description={
                props.search()
                  ? "Try a different query, or clear the search box."
                  : "Open a fetched source and click 'Process' to extract facts."
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
              <For each={rows()}>{(fact) => <FactRow fact={fact} slug={props.slug()} />}</For>
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
