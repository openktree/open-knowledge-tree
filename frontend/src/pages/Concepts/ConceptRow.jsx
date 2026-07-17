import { For, Show } from "solid-js";
import { A } from "@solidjs/router";
import Badge from "../../components/Badge";

// ConceptRow renders a single concept group in the repo-level
// Concepts list. A group is one canonical name with one or more
// contexts (L3 class labels) — the backend collapses per-context
// rows into a group so the UI shows "one concept, many contexts".
// The whole row is a link to the concept detail page
// (`/:slug/concepts/:conceptID`) addressed by the group's
// representative concept_id (the first context's id) so the URL is
// stable across the group's contexts. The contexts render as
// multiple blue Badges; the total fact count (summed across
// contexts) renders as a gray Badge.
export default function ConceptRow(props) {
  const slug = () => props.slug;
  const conceptID = () => props.concept?.contexts?.[0]?.concept_id || "";
  const detailHref = () => `/${slug()}/concepts/${conceptID()}`;
  const contexts = () => props.concept?.contexts || [];
  const totalFacts = () => props.concept?.total_fact_count ?? 0;

  return (
    <div class="border rounded dark:border-gray-700 text-sm text-gray-700 dark:text-gray-300 overflow-hidden">
      <Show
        when={!!slug() && !!conceptID()}
        fallback={
          <div class="p-3 space-y-2">
            <div class="flex items-center gap-2 flex-wrap">
              <span class="font-medium dark:text-white">{props.concept?.canonical_name}</span>
              <For each={contexts()}>{(ctx) => <Badge variant="blue">{ctx.context}</Badge>}</For>
              <Badge variant="gray">{totalFacts()} fact{totalFacts() === 1 ? "" : "s"}</Badge>
            </div>
            <Show when={props.concept?.description}>
              <div class="text-xs text-gray-500 dark:text-gray-400">{props.concept.description}</div>
            </Show>
          </div>
        }
      >
        <A
          href={detailHref()}
          class="block p-3 space-y-2 hover:bg-gray-50 dark:hover:bg-gray-800/50 transition-colors"
          title="View concept detail"
        >
          <div class="flex items-center gap-2 flex-wrap">
            <span class="font-medium dark:text-white">{props.concept?.canonical_name}</span>
            <For each={contexts()}>{(ctx) => <Badge variant="blue">{ctx.context}</Badge>}</For>
            <Badge variant="gray">{totalFacts()} fact{totalFacts() === 1 ? "" : "s"}</Badge>
          </div>
          <Show when={props.concept?.description}>
            <div class="text-xs text-gray-500 dark:text-gray-400">{props.concept.description}</div>
          </Show>
        </A>
      </Show>
    </div>
  );
}