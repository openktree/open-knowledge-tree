import { For, Show } from "solid-js";
import Alert from "../../components/Alert";
import Card from "../../components/Card";
import EmptyState from "../../components/EmptyState";
import ConfiguredBadge from "./ConfiguredBadge";
import DetailRow from "./DetailRow";

export default function DecompositionProvidersTab(props) {
  const chunkers = () => (props.providers() && props.providers().chunking) || [];
  const extractors = () => (props.providers() && props.providers().fact_extraction) || [];

  return (
    <div>
      <Alert
        variant="info"
        message="Decomposition providers run inside the source_decomposition worker. The chunker splits a source's parsed text into windows; the fact extractor pulls atomic claims out of each window. The two roles are independent — you can have a chunker and no extractor, and the worker will still process sources with zero facts."
      />

      <Section
        title="Chunking"
        emptyMessage="No chunking providers registered."
        providers={chunkers}
      />

      <Section
        title="Fact Extraction"
        emptyMessage="No fact extraction providers registered. Configure an AI provider (ollama, ollama_cloud, or openrouter) and providers.decomposition.fact_extraction to enable."
        providers={extractors}
      />
    </div>
  );
}

function Section(props) {
  return (
    <div class="mb-8">
      <h2 class="text-sm font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wide mb-3">
        {props.title}
      </h2>
      <Show
        when={props.providers().length > 0}
        fallback={
          <EmptyState
            title={props.emptyMessage}
            description="A missing decomposition provider means the source_decomposition worker skips that step. Sources are still marked processed; the pipeline degrades rather than fails."
          />
        }
      >
        <For each={props.providers()}>{(p) => <DecompositionProviderCard provider={p} />}</For>
      </Show>
    </div>
  );
}

function DecompositionProviderCard(props) {
  const p = () => props.provider;
  const configEntries = () => (p().config ? Object.entries(p().config) : []);

  return (
    <Card class="mb-4">
      <div class="flex items-start justify-between gap-3 mb-3">
        <div>
          <div class="flex items-center gap-2 flex-wrap">
            <h3 class="text-base font-semibold dark:text-white">{p().name}</h3>
            <ConfiguredBadge configured={p().configured} requires={p().requires} />
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
              <For each={p().supports}>{(s) => <span class="text-xs font-mono text-gray-700 dark:text-gray-300 bg-gray-100 dark:bg-gray-800 px-1.5 py-0.5 rounded">{s}</span>}</For>
            </div>
          </Show>
        </DetailRow>
        <DetailRow label="Requires">
          <Show
            when={p().requires}
            fallback={<span class="text-gray-400">nothing (always on)</span>}
          >
            <code class="text-xs bg-gray-100 dark:bg-gray-800 px-1.5 py-0.5 rounded break-all">
              {p().requires}
            </code>
          </Show>
        </DetailRow>
        <DetailRow label="Status">
          <Show
            when={p().configured}
            fallback={
              <span class="text-amber-600 dark:text-amber-400 text-xs">
                not configured — see the{" "}
                <code class="bg-gray-100 dark:bg-gray-800 px-1 py-0.5 rounded">
                  requires
                </code>{" "}
                field for the env var or config key to set
              </span>
            }
          >
            <span class="text-green-600 dark:text-green-400 text-xs">configured and active</span>
          </Show>
        </DetailRow>
      </div>

      <Show when={configEntries().length > 0}>
        <div class="mt-4 pt-3 border-t dark:border-gray-700">
          <p class="text-xs font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wide mb-2">
            Configuration
          </p>
          <div class="grid grid-cols-1 sm:grid-cols-2 gap-2 text-sm">
            <For each={configEntries()}>
              {([key, value]) => (
                <div class="flex items-center gap-2">
                  <span class="text-xs text-gray-500 dark:text-gray-400 font-mono">{key}</span>
                  <span class="text-xs text-gray-700 dark:text-gray-300 font-mono break-all">{value}</span>
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
