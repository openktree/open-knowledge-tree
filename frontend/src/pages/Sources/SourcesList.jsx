import { For, Show } from "solid-js";
import Button from "../../components/Button";
import Card from "../../components/Card";
import EmptyState from "../../components/EmptyState";
import Pagination from "../../components/Pagination";
import SearchInput from "../../components/SearchInput";
import SourceRow from "./SourceRow";

// SourcesList renders the repo-scoped source list. The parent
// (Sources/index.jsx) owns the resource, the search query, and
// the offset; this component is controlled via props and calls
// back via onSearch/onOffsetChange. The resource now returns a
// pageEnvelope {data, total, limit, offset} so the row list is
// props.sources()?.data and the paging bar reads total/limit.
export default function SourcesList(props) {
  const rows = () => props.sources()?.data || [];
  return (
    <Card>
      <div class="flex items-center justify-between mb-3 gap-3 flex-wrap">
        <h2 class="text-lg font-semibold dark:text-white">Fetched sources</h2>
        <div class="flex items-center gap-2">
          <SearchInput
            placeholder="Search url, title, or DOI..."
            initial={props.search()}
            onSearch={props.onSearch}
          />
          <Button
            variant="secondary"
            onClick={props.onRefresh}
            class="text-xs px-2 py-1"
            loading={props.loading()}
            loadingText="Refreshing..."
          >
            Refresh
          </Button>
        </div>
      </div>
      <p class="text-sm text-gray-500 dark:text-gray-400 mb-4">
        Sources created in this repository. Open a row to read the extracted content, view images,
        and copy a shareable link.
      </p>

      <Show
        when={props.hasRepo()}
        fallback={
          <EmptyState
            title="Select a repository to view its sources."
            description="Use the repository dropdown in the top bar."
          />
        }
      >
        <Show
          when={!props.loading()}
          fallback={<p class="text-sm text-gray-400 dark:text-gray-500">Loading sources...</p>}
        >
          <Show
            when={rows().length > 0}
            fallback={
              <EmptyState
                title={props.search() ? "No sources match your search." : "No sources yet."}
                description={
                  props.search()
                    ? "Try a different query, or clear the search box."
                    : "Add one above or use the Providers page to search and retrieve content."
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
                  {(source) => (
                    <SourceRow
                      source={source}
                      slug={props.slug()}
                      repoID={props.repoID()}
                      canProcess={
                        props.canProcess() &&
                        source.status === "fetched" &&
                        source.parsed_text &&
                        source.parsed_text.trim().length > 0
                      }
                      processDisabled={source.status === "processed"}
                      processing={props.processingID() === source.id}
                      onProcess={() => props.onProcess(source)}
                      canReprocess={props.canReprocess()}
                      reprocessing={props.reprocessingID() === source.id}
                      onReprocess={() => props.onReprocess(source)}
                      canRetry={props.canRetry() && source.status === "failed"}
                      retrying={props.retryingID() === source.id}
                      onRetry={() => props.onRetry(source)}
                      canDelete={props.canDelete()}
                      deleting={props.deletingID() === source.id}
                      onDelete={() => props.onDelete(source)}
                    />
                  )}
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
      </Show>
    </Card>
  );
}
