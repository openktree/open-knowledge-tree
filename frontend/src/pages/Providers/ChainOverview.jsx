import { For, Show } from "solid-js";
import Card from "../../components/Card";
import EmptyState from "../../components/EmptyState";

export default function ChainOverview(props) {
  return (
    <Card class="mb-6">
      <h2 class="text-lg font-semibold mb-1 dark:text-white">
        Fetch chain overview
      </h2>
      <p class="text-sm text-gray-500 dark:text-gray-400 mb-4">
        How a source URL or DOI is resolved into a fetched body. Providers
        are tried top-to-bottom; the first match wins. The chain is built
        at server startup and reflects the current configuration.
      </p>
      <Show
        when={props.providers().length > 0}
        fallback={
          <EmptyState
            title="No fetch providers are wired up."
            description="This usually means a misconfigured server. Check the API logs for resolution-provider initialization errors."
          />
        }
      >
        <ol class="space-y-2">
          <For each={props.providers()}>
            {(p, i) => (
              <li class="flex items-center gap-3 text-sm">
                <span class="inline-flex items-center justify-center w-6 h-6 rounded-full bg-blue-100 dark:bg-blue-900 text-blue-700 dark:text-blue-300 text-xs font-semibold">
                  {i() + 1}
                </span>
                <span class="font-medium dark:text-gray-200">{p.name}</span>
                <span class="font-mono text-xs text-gray-400 dark:text-gray-500">
                  {p.id}
                </span>
                <Show
                  when={p.configured}
                  fallback={
                    <span class="text-xs text-amber-600 dark:text-amber-400">
                      not configured
                    </span>
                  }
                >
                  <span class="text-xs text-green-600 dark:text-green-400">
                    active
                  </span>
                </Show>
              </li>
            )}
          </For>
        </ol>
      </Show>
    </Card>
  );
}
