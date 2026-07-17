import { For, Show } from "solid-js";
import Card from "../../components/Card";
import Button from "../../components/Button";
import EmptyState from "../../components/EmptyState";
import Pagination from "../../components/Pagination";
import SearchInput from "../../components/SearchInput";
import RemoteRow from "./RemoteRow";
import RemoteDetailDialog from "./RemoteDetailDialog";

export default function RemoteContent(props) {
  const rows = () => props.sources()?.sources || [];
  return (
    <>
      <Card>
        <div class="flex items-center justify-between mb-3 gap-3 flex-wrap">
          <h2 class="text-lg font-semibold dark:text-white">Remote sources</h2>
          <div class="flex items-center gap-2 flex-wrap">
            <Show when={props.canManage?.()}>
              <Button
                variant="secondary"
                onClick={props.onPushAll}
                loading={props.busyPushAll()}
                loadingText="Enqueuing…"
                class="text-xs"
              >
                Push All to Registry
              </Button>
              <Button
                variant="secondary"
                onClick={props.onPullAll}
                loading={props.busyPullAll()}
                loadingText="Enqueuing…"
                class="text-xs"
              >
                Pull All from Registry
              </Button>
            </Show>
            <Show when={rows().length > 0}>
              <Button
                variant="secondary"
                onClick={props.onPullPage}
                loading={props.busyPullPage()}
                loadingText="Enqueuing…"
                class="text-xs"
              >
                Pull Page ({rows().length})
              </Button>
              <Button
                variant="secondary"
                onClick={props.onPullAllResults}
                loading={props.busyPullAllResults()}
                loadingText="Collecting…"
                class="text-xs"
              >
                Pull All Results ({props.total()?.toLocaleString()})
              </Button>
            </Show>
            <SearchInput
              placeholder="Search title, url, or DOI..."
              initial={props.search()}
              onSearch={props.onSearch}
            />
          </div>
        </div>
        <p class="text-sm text-gray-500 dark:text-gray-400 mb-4">
          Sources available on the remote knowledge registry. Click a row to browse its
          metadata and available decompositions, or click "Pull" to import a source with its
          facts and concepts into this repository. "Pull Page" imports every source on the
          current page; "Pull All Results" paginates through every source matching your
          search and imports them all.
        </p>

        <Show
          when={props.hasRepo()}
          fallback={
            <EmptyState
              title="Select a repository to browse remote sources."
              description="Use the repository dropdown in the top bar."
            />
          }
        >
          <Show
            when={!props.loading()}
            fallback={
              <p class="text-sm text-gray-400 dark:text-gray-500">Loading remote sources...</p>
            }
          >
            <Show
              when={rows().length > 0}
              fallback={
                <EmptyState
                  title={props.search() ? "No remote sources match your search." : "No remote sources found."}
                  description={props.search() ? "Try a different query, or clear the search box." : "The registry appears to be empty or unreachable."}
                />
              }
            >
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
                  {(src) => (
                    <RemoteRow
                      source={src}
                      exists={src.exists}
                      pullingID={props.pullingID}
                      onPull={props.onPull}
                      onOpenDetail={props.onOpenDetail}
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
            </Show>
          </Show>
        </Show>
      </Card>
      <Show when={props.selectedSource()}>
        <RemoteDetailDialog
          source={props.selectedSource()}
          slug={props.slug()}
          pullingID={props.pullingID}
          onPull={props.onPull}
          onClose={props.onCloseDetail}
        />
      </Show>
    </>
  );
}
