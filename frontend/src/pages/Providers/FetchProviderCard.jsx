import { For, Show } from "solid-js";
import Badge from "../../components/Badge";
import Card from "../../components/Card";
import ConfiguredBadge from "./ConfiguredBadge";
import DetailRow from "./DetailRow";

export default function FetchProviderCard(props) {
  const p = () => props.provider;

  return (
    <Card class="mb-6">
      <div class="flex items-start justify-between gap-3 mb-3">
        <div>
          <div class="flex items-center gap-2 flex-wrap">
            <h2 class="text-lg font-semibold dark:text-white">{p().name}</h2>
            <ConfiguredBadge configured={p().configured} requires={p().requires} />
            <Show when={p().priority}>
              <Badge variant="gray">priority {p().priority}</Badge>
            </Show>
            <Show when={p().enabled_for_repo === false}>
              <Badge variant="gray">Disabled for this repo</Badge>
            </Show>
          </div>
          <p class="text-xs text-gray-400 dark:text-gray-500 font-mono mt-0.5">{p().id}</p>
        </div>
      </div>

      <Show when={p().description}>
        <p class="text-sm text-gray-700 dark:text-gray-300 mb-4">{p().description}</p>
      </Show>

      <div class="grid grid-cols-1 sm:grid-cols-2 gap-3 text-sm">
        <DetailRow label="Supports">
          <Show
            when={p().supports && p().supports.length > 0}
            fallback={<span class="text-gray-400">none</span>}
          >
            <div class="flex items-center gap-1 flex-wrap">
              <For each={p().supports}>{(s) => <Badge variant="blue">{s}</Badge>}</For>
            </div>
          </Show>
        </DetailRow>
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
            <span class="text-green-600 dark:text-green-400 text-xs">configured and active</span>
          </Show>
        </DetailRow>
      </div>

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
