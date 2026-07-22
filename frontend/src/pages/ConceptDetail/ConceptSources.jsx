import { A } from "@solidjs/router";
import { createResource, createSignal, For, Show } from "solid-js";
import Badge from "../../components/Badge";
import Card from "../../components/Card";
import EmptyState from "../../components/EmptyState";
import { api } from "../../services/api";
import { CONCEPT_SOURCES_BATCH, CONCEPT_SOURCES_PREVIEW } from "./constants";

// ConceptSources renders the unique sources backing the active
// context's facts, with a fact_count per source as a provenance
// signal. It is scoped per-context (keyed on the active context's
// concept_id, re-fetching on tab switch) and sits above the facts
// section in ContextPanel so a reader sees "where do these claims
// come from?" before "what are the claims?".
//
// A single batch (CONCEPT_SOURCES_BATCH, default 100) is fetched up
// front; the top CONCEPT_SOURCES_PREVIEW (default 10) are shown and a
// "Show N more" button reveals the rest of the batch client-side
// (instant expand, no extra network round-trip). When the total
// exceeds the batch, a note links to the full source list.
export default function ConceptSources(props) {
  const slug = () => props.slug;
  const conceptID = () => props.conceptID;

  const [showAll, setShowAll] = createSignal(false);
  const [srcData] = createResource(
    () => ({ slug: slug(), conceptID: conceptID() }),
    async ({ slug, conceptID }) => {
      if (!slug || !conceptID) return { data: [], total: 0 };
      try {
        return await api.listConceptSources(slug, conceptID, {
          limit: CONCEPT_SOURCES_BATCH,
          offset: 0,
        });
      } catch {
        return { data: [], total: 0 };
      }
    },
  );

  const sources = () => srcData()?.data || [];
  const total = () => srcData()?.total || 0;
  const visible = () => (showAll() ? sources() : sources().slice(0, CONCEPT_SOURCES_PREVIEW));
  const overflow = () => Math.max(0, sources().length - CONCEPT_SOURCES_PREVIEW);
  const truncatedNote = () => Math.max(0, total() - sources().length);

  return (
    <Card>
      <div class="flex items-center gap-2 mb-4 flex-wrap">
        <h2 class="text-lg font-semibold dark:text-white">Sources</h2>
        <Badge variant="gray">
          {total().toLocaleString()} source{total() === 1 ? "" : "s"}
        </Badge>
      </div>

      <Show
        when={sources().length > 0}
        fallback={
          <EmptyState
            title="No sources linked to this context yet."
            description="Once facts are processed and concepts extracted, the sources backing this concept's facts will appear here."
          />
        }
      >
        <div class="space-y-2">
          <For each={visible()}>
            {(src) => {
              const title = () => src.parsed_title || src.url || "Untitled source";
              const doiUrl = () => (src.doi ? `https://doi.org/${src.doi}` : "");
              return (
                <div class="border rounded dark:border-gray-700 p-3 text-sm">
                  <div class="flex items-start justify-between gap-3">
                    <div class="min-w-0 flex-1">
                      <Show when={slug() && src.id}>
                        <A
                          href={`/${slug()}/sources/${src.id}`}
                          class="font-medium text-blue-600 hover:underline dark:text-blue-400 break-all"
                          title="Open source detail"
                        >
                          {title()}
                        </A>
                      </Show>
                      <Show when={!(slug() && src.id)}>
                        <span class="font-medium dark:text-white break-all">{title()}</span>
                      </Show>
                      <div class="text-xs text-gray-500 dark:text-gray-400 mt-1 break-all">
                        {src.url}
                      </div>
                      <Show when={src.parsed_author}>
                        <div class="text-xs text-gray-500 dark:text-gray-400 mt-1">
                          {src.parsed_author}
                        </div>
                      </Show>
                      <div class="flex items-center gap-2 mt-2 flex-wrap">
                        <Badge variant="gray">
                          {src.fact_count} fact{src.fact_count === 1 ? "" : "s"}
                        </Badge>
                        <Show when={src.doi}>
                          <a
                            href={doiUrl()}
                            target="_blank"
                            rel="noopener noreferrer"
                            class="text-xs text-blue-600 dark:text-blue-400 hover:underline"
                          >
                            DOI: {src.doi} ↗
                          </a>
                        </Show>
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

        <Show when={overflow() > 0 && !showAll()}>
          <button
            class="text-xs text-blue-600 dark:text-blue-400 hover:underline mt-3"
            onClick={() => setShowAll(true)}
          >
            Show {overflow()} more
          </button>
        </Show>
        <Show when={showAll() && truncatedNote() > 0}>
          <p class="text-xs text-gray-500 dark:text-gray-400 mt-3">
            Showing first {sources().length} of {total().toLocaleString()} sources.
          </p>
        </Show>
        <Show when={!showAll() && truncatedNote() > 0 && overflow() === 0}>
          <p class="text-xs text-gray-500 dark:text-gray-400 mt-3">
            Showing {sources().length} of {total().toLocaleString()} sources.
          </p>
        </Show>
      </Show>
    </Card>
  );
}
