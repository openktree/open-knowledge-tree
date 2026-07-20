import { For, Show } from "solid-js";
import Badge from "../../components/Badge";
import Card from "../../components/Card";
import EmptyState from "../../components/EmptyState";
import ConfiguredBadge from "./ConfiguredBadge";
import DetailRow from "./DetailRow";

// EmbeddingProvidersTab renders the active embedding configuration
// and the list of AI providers that implement ai.EmbeddingProvider
// (chat-only providers are filtered out by the backend). Switching
// the embedding model or dimensions requires re-embedding existing
// facts (the Qdrant collection must be recreated), so the UI only
// surfaces the active config — operators edit the YAML to change it.
export default function EmbeddingProvidersTab(props) {
  const data = () => props.data();
  const active = () => (data() && data().active) || null;
  const providers = () => (data() && data().providers) || [];

  return (
    <div>
      <Show when={active()}>
        <ActiveConfigCard active={active()} />
      </Show>

      <Show
        when={providers().length > 0}
        fallback={
          <EmptyState
            title="No embedding-capable providers configured."
            description="Register an AI provider that implements ai.EmbeddingProvider (ollama or openrouter) and restart the API."
          />
        }
      >
        <h2 class="text-sm font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wide mt-8 mb-3">
          Embedding-capable providers ({providers().length})
        </h2>
        <For each={providers()}>{(p) => <EmbeddingProviderCard provider={p} />}</For>
      </Show>
    </div>
  );
}

function ActiveConfigCard(props) {
  const a = () => props.active;

  return (
    <Card class="mb-6 border-l-4 border-blue-400 dark:border-blue-500">
      <div class="flex items-start justify-between gap-3 mb-3">
        <div>
          <h2 class="text-lg font-semibold dark:text-white">Active embedding configuration</h2>
          <p class="text-xs text-gray-500 dark:text-gray-400 mt-0.5">
            The model used by the embed_facts worker to vectorize facts into Qdrant.
          </p>
        </div>
        <Show when={a().configured} fallback={<Badge variant="red">not configured</Badge>}>
          <Badge variant="green">active</Badge>
        </Show>
      </div>

      <div class="grid grid-cols-1 sm:grid-cols-3 gap-3 text-sm">
        <DetailRow label="Provider">
          <code class="font-mono text-xs text-gray-700 dark:text-gray-300">
            {a().provider || "\u2014"}
          </code>
        </DetailRow>
        <DetailRow label="Model">
          <code class="font-mono text-xs text-gray-700 dark:text-gray-300">
            {a().model || "\u2014"}
          </code>
        </DetailRow>
        <DetailRow label="Dimensions">
          <span class="font-mono text-xs text-gray-700 dark:text-gray-300">
            {a().dimensions || "\u2014"}
          </span>
        </DetailRow>
      </div>

      <div class="mt-4 pt-3 border-t dark:border-gray-700">
        <p class="text-xs text-amber-600 dark:text-amber-400">
          Switching the embedding model or dimensions requires re-embedding existing facts (the
          Qdrant collection must be recreated). Edit the YAML and restart the API to change it.
        </p>
      </div>
    </Card>
  );
}

function EmbeddingProviderCard(props) {
  const p = () => props.provider;

  return (
    <Card class="mb-4">
      <div class="flex items-start justify-between gap-3 mb-3">
        <div>
          <div class="flex items-center gap-2 flex-wrap">
            <h3 class="text-base font-semibold dark:text-white">{p().name}</h3>
            <ConfiguredBadge configured={p().configured} requires={p().requires} />
            <Show when={p().embedding_capable}>
              <Badge variant="purple">embedding capable</Badge>
            </Show>
          </div>
          <p class="text-xs text-gray-400 dark:text-gray-500 font-mono mt-0.5">{p().id}</p>
        </div>
      </div>

      <Show when={p().description}>
        <p class="text-sm text-gray-700 dark:text-gray-300 mb-3">{p().description}</p>
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
