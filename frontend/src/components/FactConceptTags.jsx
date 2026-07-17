import { For, Show, createResource } from "solid-js";
import { A } from "@solidjs/router";
import { api } from "../services/api";
import Badge from "./Badge";

// FactConceptTags shows the concepts linked to a fact as a row of
// badges, each linking to the concept detail page. Used on the
// FactDetail page so the user can see which concepts were extracted
// from the fact and jump to the "all facts for this concept" view.
//
// When `props.concepts` is provided (the getFact response now carries
// the linked concepts inline), the component renders them directly
// without an extra network request — a consolidation that keeps the
// page in sync with the MCP getFact shape. When `props.concepts` is
// absent (e.g. a future caller that doesn't fetch the full fact), it
// falls back to the dedicated listFactConcepts endpoint.
export default function FactConceptTags(props) {
  const [fetched] = createResource(
    () => ({ slug: props.slug, factID: props.factID, key: props.refreshKey?.() ?? 0 }),
    async ({ slug, factID }) => {
      if (!slug || !factID) return [];
      try {
        const res = await api.listFactConcepts(slug, factID);
        return res?.data || [];
      } catch {
        return [];
      }
    }
  );

  const concepts = () => (props.concepts ? props.concepts() : fetched());

  return (
    <div class="border border-border rounded p-4 mb-4">
      <div class="font-semibold text-text-base mb-2 text-xs">Concepts</div>
      <Show
        when={!props.concepts && fetched.loading}
        fallback={
          <Show
            when={(concepts() || []).length > 0}
            fallback={
              <p class="text-sm text-text-muted">
                No concepts linked to this fact yet.
              </p>
            }
          >
            <div class="flex flex-wrap gap-2">
              <For each={concepts()}>
                {(concept) => (
                  <A href={`/${props.slug}/concepts/${concept.id}`} class="inline-flex items-center gap-1">
                    <Badge variant="blue">{concept.canonical_name}</Badge>
                    <span class="text-xs text-text-muted">{concept.context}</span>
                  </A>
                )}
              </For>
            </div>
          </Show>
        }
      >
        <p class="text-sm text-text-muted">Loading concepts...</p>
      </Show>
    </div>
  );
}