import { createResource, createSignal, For, Show } from "solid-js";
import { api } from "../../services/api";
import Card from "../../components/Card";

// PromptsetPanel is the per-repo promptset selection section of the
// RepositorySettings page. It loads the repo's active + accepted
// hashes plus the catalog of available promptsets, and lets the
// admin pick the active philosophy + the additionally-accepted set
// (the cache-admit set for registry pull).
//
// Props:
//   repoID  – accessor string  repository UUID (or slug)
//   onAlert – (alert) => void
export default function PromptsetPanel(props) {
  const [ps, { refetch }] = createResource(props.repoID, (id) =>
    id ? api.getRepositoryPromptset(id).catch((e) => {
      props.onAlert?.({ variant: "error", message: e.message });
      return null;
    }) : null
  );
  const [catalog, { refetch: refetchCatalog }] = createResource(() => api.listPromptsets().catch(() => []));
  const [busy, setBusy] = createSignal(false);

  const active = () => ps()?.active_hash ?? "";
  const accepted = () => ps()?.accepted_hashes ?? [];
  const effective = () => ps()?.effective_hash ?? "";
  const globalDefault = () => ps()?.global_default_hash ?? "";

  const isAccepted = (hash) => accepted().includes(hash);

  const save = async (nextActive, nextAccepted) => {
    setBusy(true);
    try {
      await api.setRepositoryPromptset(props.repoID(), {
        active_hash: nextActive || null,
        accepted_hashes: nextAccepted,
      });
      props.onAlert?.({ variant: "success", message: "Promptset updated." });
      refetch();
    } catch (err) {
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setBusy(false);
    }
  };

  const changeActive = (hash) => {
    // Keep the active hash in the accepted set.
    const next = new Set(accepted());
    if (hash) next.add(hash);
    save(hash, Array.from(next));
  };

  const toggleAccepted = (hash, checked) => {
    const next = new Set(accepted());
    if (checked) next.add(hash);
    else next.delete(hash);
    // The active hash is always accepted; don't let the caller
    // remove it via the checkbox.
    if (active() && !next.has(active())) next.add(active());
    save(active(), Array.from(next));
  };

  return (
    <Card>
      <h3 class="text-lg font-semibold mb-1 dark:text-white">Promptset</h3>
      <p class="text-sm text-gray-500 dark:text-gray-400 mb-4">
        Choose the philosophy this repository decomposes under. The active promptset runs for new
        decompositions; the accepted set controls which foreign decompositions the registry cache may
        pull in without contaminating the graph.
      </p>
      <Show when={!ps.loading && !catalog.loading}>
        <div class="space-y-4">
          <div>
            <label class="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">
              Active promptset
            </label>
            <select
              disabled={busy()}
              value={active()}
              onChange={(e) => changeActive(e.currentTarget.value)}
              class="text-sm px-3 py-2 rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-700 dark:text-gray-200 disabled:opacity-50"
            >
              <option value="">
                Inherit global default{globalDefault() ? ` (${globalDefault().slice(0, 12)}…)` : ""}
              </option>
              <For each={catalog() ?? []}>
                {(p) => <option value={p.hash}>{p.name} ({p.source})</option>}
              </For>
            </select>
            <p class="text-xs text-gray-400 dark:text-gray-500 mt-1">
              Effective: <span class="font-mono">{effective().slice(0, 16)}…</span>
            </p>
          </div>
          <div>
            <label class="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">
              Additionally accepted (registry pull)
            </label>
            <p class="text-xs text-gray-400 dark:text-gray-500 mb-2">
              Decompositions with these hashes may be pulled from the registry. The active hash is always accepted.
            </p>
            <div class="space-y-1">
              <For each={catalog() ?? []}>
                {(p) => (
                  <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
                    <input
                      type="checkbox"
                      disabled={busy() || p.hash === active()}
                      checked={isAccepted(p.hash)}
                      onChange={(e) => toggleAccepted(p.hash, e.currentTarget.checked)}
                      class="rounded"
                    />
                    <span>{p.name}</span>
                    <span class="text-xs text-gray-400 dark:text-gray-500">({p.source})</span>
                  </label>
                )}
              </For>
            </div>
          </div>
        </div>
      </Show>
    </Card>
  );
}