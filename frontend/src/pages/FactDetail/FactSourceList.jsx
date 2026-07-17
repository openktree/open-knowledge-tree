import { For, Show } from "solid-js";
import { A } from "@solidjs/router";

// FactSourceList renders the sources that extracted or confirmed
// a fact. Each row shows the source's parsed title (or URL when
// the title is missing), the chunk index the fact was extracted
// from, and a link to open the source detail page + the external
// URL. The user validates the fact against each source by opening
// the external URL and confirming the fact was actually extracted
// from that source's chunk.
//
// The first_seen_at column shows when the link was established —
// a dedup merge adds rows here, so a fact confirmed by 10 sources
// shows 10 rows with varying first_seen_at values.
export default function FactSourceList(props) {
  const sources = () => props.sources() || [];
  const fmt = (ts) => (ts ? new Date(ts).toLocaleString() : "");

  return (
    <div>
      <h3 class="text-sm font-semibold dark:text-white mb-2">
        Sources ({sources().length})
      </h3>
      <div class="space-y-2">
        <For each={sources()}>
          {(src) => {
            const title = () => src.parsed_title || src.url || "Untitled source";
            const chunk = () => src.chunk_index;
            return (
              <div class="border rounded dark:border-gray-700 p-3 text-sm">
                <div class="flex items-start justify-between gap-3">
                  <div class="min-w-0">
                    <Show when={props.slug && src.source_id}>
                      <A
                        href={`/${props.slug}/sources/${src.source_id}`}
                        class="font-medium text-blue-600 hover:underline dark:text-blue-400 break-all"
                        title="Open source detail"
                      >
                        {title()}
                      </A>
                    </Show>
                    <Show when={!(props.slug && src.source_id)}>
                      <span class="font-medium dark:text-white break-all">{title()}</span>
                    </Show>
                    <div class="text-xs text-gray-500 dark:text-gray-400 mt-1 break-all">
                      {src.url}
                    </div>
                    <div class="text-xs text-gray-500 dark:text-gray-400 mt-1">
                      chunk {chunk()} · first seen {fmt(src.first_seen_at)}
                    </div>
                  </div>
                  <Show when={src.url}>
                    <a
                      href={src.url}
                      target="_blank"
                      rel="noopener noreferrer"
                      class="text-xs px-2 py-1 rounded border border-gray-300 dark:border-gray-600 hover:bg-gray-100 dark:hover:bg-gray-700 text-gray-700 dark:text-gray-300 whitespace-nowrap"
                      title="Open external source"
                    >
                      Open ↗
                    </a>
                  </Show>
                </div>
              </div>
            );
          }}
        </For>
      </div>
    </div>
  );
}