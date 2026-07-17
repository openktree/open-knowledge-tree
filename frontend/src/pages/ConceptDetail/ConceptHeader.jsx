import { For, Show } from "solid-js";
import Badge from "../../components/Badge";
import Button from "../../components/Button";
import Card from "../../components/Card";

// ConceptHeader renders the group-level header card for a concept
// group: the canonical name, the total fact count summed across
// every context, a row of context tabs (one per context entry), and
// the active context's metadata + aliases. The active tab is
// controlled by the parent via activeIndex / onSelectContext. Each
// tab shows the context's L3 class label and its per-context fact
// count. The metadata and aliases update when the active context
// changes (passed via activeContext).
export default function ConceptHeader(props) {
  const contexts = () => props.group?.contexts || [];
  const totalFacts = () => props.group?.total_fact_count ?? 0;
  const activeIndex = () => props.activeIndex ?? 0;
  const activeContext = () => props.activeContext;
  const aliases = () => activeContext()?.aliases || [];

  return (
    <Card>
      <div class="flex items-center justify-between mb-3">
        <h2 class="text-lg font-semibold dark:text-white">Concept detail</h2>
        <Button variant="secondary" onClick={props.onRefresh} class="text-xs px-2 py-1">
          Refresh
        </Button>
      </div>
      <div class="border rounded dark:border-gray-700 p-4 mb-4 text-sm text-gray-700 dark:text-gray-300 space-y-3">
        <div class="flex items-center gap-2 flex-wrap">
          <span class="text-base font-medium dark:text-white">{props.group?.canonical_name}</span>
          <Badge variant="gray">{totalFacts().toLocaleString()} fact{totalFacts() === 1 ? "" : "s"}</Badge>
          <Show when={(contexts()?.length ?? 0) > 1}>
            <Badge variant="blue">{contexts().length} contexts</Badge>
          </Show>
        </div>
        <div class="flex flex-wrap gap-2">
          <For each={contexts()}>
            {(ctx, i) => (
              <button
                type="button"
                onClick={() => props.onSelectContext?.(i())}
                class={`px-2 py-1 rounded text-xs border transition-colors ${
                  i() === activeIndex()
                    ? "bg-blue-600 text-white border-blue-600"
                    : "bg-white dark:bg-gray-800 text-gray-700 dark:text-gray-300 border-gray-300 dark:border-gray-600 hover:bg-gray-50 dark:hover:bg-gray-700"
                }`}
                title={`Show ${ctx.context} context (${ctx.fact_count} facts)`}
              >
                {ctx.context}
                <span class={`ml-1 ${i() === activeIndex() ? "text-blue-100" : "text-gray-400"}`}>
                  ({ctx.fact_count})
                </span>
              </button>
            )}
          </For>
        </div>
      </div>

      <div class="border rounded dark:border-gray-700 p-4 mb-4 text-xs text-gray-600 dark:text-gray-400 space-y-1">
        <div class="font-semibold text-gray-700 dark:text-gray-300 mb-1">Metadata</div>
        <Show when={activeContext()?.created_at}>
          <div>Created: {new Date(activeContext().created_at).toLocaleString()}</div>
        </Show>
        <Show when={activeContext()?.embedded_model}>
          <div>Embedded model: {activeContext().embedded_model}</div>
        </Show>
        <Show when={activeContext()?.embedded_at}>
          <div>Embedded at: {new Date(activeContext().embedded_at).toLocaleString()}</div>
        </Show>
      </div>

      <Show when={aliases().length > 0}>
        <div class="border rounded dark:border-gray-700 p-4 mb-4 relative group">
          <div class="font-semibold text-gray-700 dark:text-gray-300 mb-2 text-xs">Aliases ({aliases().length})</div>
          <div class="flex flex-wrap gap-2">
            <For each={aliases().slice(0, 10)}>{(alias) => <Badge variant="gray">{alias}</Badge>}</For>
            <Show when={aliases().length > 10}>
              <span class="text-xs px-2 py-0.5 text-gray-400 dark:text-gray-500 cursor-help">
                +{aliases().length - 10} more
              </span>
            </Show>
          </div>
          <Show when={aliases().length > 10}>
            <div class="absolute left-0 right-0 top-full mt-1 z-30 hidden group-hover:block">
              <div class="bg-white dark:bg-gray-800 border border-gray-200 dark:border-gray-600 rounded shadow-lg p-3">
                <div class="flex flex-wrap gap-1.5">
                  <For each={aliases().slice(10)}>{(alias) => <Badge variant="gray">{alias}</Badge>}</For>
                </div>
              </div>
            </div>
          </Show>
        </div>
      </Show>
    </Card>
  );
}