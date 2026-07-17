import { For, Show } from "solid-js";
import Badge from "../../components/Badge";
import Card from "../../components/Card";
import EmptyState from "../../components/EmptyState";
import ConfiguredBadge from "./ConfiguredBadge";
import DetailRow from "./DetailRow";

export default function AIProvidersTab(props) {
  const aiProviders = () => props.aiProviders() || [];

  return (
    <div>
      <Show
        when={aiProviders().length > 0}
        fallback={
          <EmptyState
            title="No AI providers configured."
            description="Set the required environment variables and restart the API to enable AI providers."
          />
        }
      >
        <For each={aiProviders()}>
          {(p) => <AIProviderCard provider={p} />}
        </For>
      </Show>
    </div>
  );
}

function AIProviderCard(props) {
  const p = () => props.provider;

  return (
    <Card class="mb-6">
      <div class="flex items-start justify-between gap-3 mb-3">
        <div>
          <div class="flex items-center gap-2 flex-wrap">
            <h2 class="text-lg font-semibold dark:text-white">{p().name}</h2>
            <ConfiguredBadge configured={p().configured} requires={p().requires} />
          </div>
          <p class="text-xs text-gray-400 dark:text-gray-500 font-mono mt-0.5">
            {p().id}
          </p>
        </div>
      </div>

      <Show when={p().description}>
        <p class="text-sm text-gray-700 dark:text-gray-300 mb-4">
          {p().description}
        </p>
      </Show>

      <div class="grid grid-cols-1 sm:grid-cols-2 gap-3 text-sm">
        <DetailRow label="Timeout">
          <span class="font-mono text-xs text-gray-700 dark:text-gray-300">
            {p().timeout || "\u2014"}
          </span>
        </DetailRow>
        <DetailRow label="Requires">
          <Show
            when={p().requires}
            fallback={<span class="text-gray-400">nothing (always on)</span>}
          >
            <code class="text-xs bg-gray-100 dark:bg-gray-800 px-1.5 py-0.5 rounded">
              {p().requires}
            </code>
          </Show>
        </DetailRow>
        <DetailRow label="Status">
          <Show
            when={p().configured}
            fallback={
              <span class="text-amber-600 dark:text-amber-400 text-xs">
                not configured — set{" "}
                <code class="bg-gray-100 dark:bg-gray-800 px-1 py-0.5 rounded">
                  {p().requires || "the required env var"}
                </code>{" "}
                and restart the API
              </span>
            }
          >
            <span class="text-green-600 dark:text-green-400 text-xs">
              configured and active
            </span>
          </Show>
        </DetailRow>
      </div>

      <Show when={p().models && p().models.length > 0}>
        <div class="mt-4 pt-3 border-t dark:border-gray-700">
          <p class="text-xs font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wide mb-2">
            Models ({p().models.length})
          </p>
          <div class="space-y-2">
            <For each={p().models}>
              {(m) => (
                <div class="flex items-center gap-2 text-sm border rounded dark:border-gray-700 p-2">
                  <span class="font-mono text-xs font-medium dark:text-gray-200 flex-1">
                    {m.id}
                  </span>
                  <Show when={m.input_cost_per_1m > 0 || m.output_cost_per_1m > 0}>
                    <Badge variant="gray">
                      ${m.input_cost_per_1m}/1M in · ${m.output_cost_per_1m}/1M out
                    </Badge>
                  </Show>
                  <Show when={m.thinking_level}>
                    <Badge variant="purple">think: {m.thinking_level}</Badge>
                  </Show>
                </div>
              )}
            </For>
          </div>
        </div>
      </Show>

      <Show when={p().notes}>
        <div class="mt-4 pt-3 border-t dark:border-gray-700">
          <p class="text-xs font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wide mb-1">
            Notes
          </p>
          <p class="text-sm text-gray-600 dark:text-gray-400">{p().notes}</p>
        </div>
      </Show>
    </Card>
  );
}
