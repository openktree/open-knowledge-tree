import { createSignal, Show, For } from "solid-js";
import { api } from "../../services/api";
import Card from "../../components/Card";

// RegistryPanel is the per-repo registry integration card on the
// Repository Settings page. It bundles three controls:
//
//   1. The on/off toggle for the registry integration (cache lookup
//      on fetch + remote browse/pull). When off, the retrieve_source
//      worker skips the registry cache and the /remote endpoints
//      return 503 for this repo.
//   2. The registry selector (a dropdown of the configured registry
//      ids from `registry_options`). Only shown when more than one
//      registry is configured.
//   3. The auto-contribute toggle (autopush). When on, the
//      cleanup_facts worker chains a contribute_source job for every
//      source that finishes processing, sharing it publicly via the
//      remote registry. Requires the integration to be enabled.
//
// Props:
//   - repoID:              () => string
//   - registryID:          () => string
//   - registryEnabled:     () => boolean
//   - registryOptions:     () => string[]
//   - registryConfigured:  () => boolean
//   - autoContribute:      () => boolean
//   - onChanged:           () => void  — refetch settings after toggle
//   - onAlert:             (alert) => void
export default function RegistryPanel(props) {
  const [busyToggle, setBusyToggle] = createSignal(false);
  const [busySelect, setBusySelect] = createSignal(false);
  const [busyAuto, setBusyAuto] = createSignal(false);
  const [busyModels, setBusyModels] = createSignal(false);

  const configured = () => props.registryConfigured?.() !== false;
  const enabled = () => !!props.registryEnabled?.();
  const autoOn = () => !!props.autoContribute?.();
  const options = () => props.registryOptions?.() ?? [];
  const currentID = () => props.registryID?.() ?? "default";
  const allowedModels = () => props.allowedModels?.() ?? null;
  const allowedModelsDefault = () => props.allowedModelsDefault?.() ?? [];
  const catalog = () => props.catalog?.() ?? [];

  const isPerRepo = () => Array.isArray(allowedModels());

  const toggleEnabled = async () => {
    setBusyToggle(true);
    try {
      await api.setRepositoryRegistry(props.repoID(), { enabled: !enabled() });
      props.onChanged?.();
    } catch (err) {
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setBusyToggle(false);
    }
  };

  const changeRegistry = async (e) => {
    const id = e.currentTarget.value;
    setBusySelect(true);
    try {
      await api.setRepositoryRegistry(props.repoID(), { registry_id: id });
      props.onChanged?.();
    } catch (err) {
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setBusySelect(false);
    }
  };

  const toggleAuto = async () => {
    setBusyAuto(true);
    try {
      await api.setAutoContribute(props.repoID(), !autoOn());
      props.onChanged?.();
    } catch (err) {
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setBusyAuto(false);
    }
  };

  const saveAllowedModels = async (models) => {
    setBusyModels(true);
    try {
      const body = models === null
        ? { allowed_models: null }
        : { allowed_models: models };
      await api.setRepositoryRegistry(props.repoID(), body);
      props.onChanged?.();
    } catch (err) {
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setBusyModels(false);
    }
  };

  return (
    <Card>
      <h3 class="text-lg font-semibold mb-3 dark:text-white">Remote Registry</h3>
      <Show
        when={configured()}
        fallback={
          <p class="text-sm text-yellow-600 dark:text-yellow-400">
            No registry is configured. Set <code>providers.registry.url</code> or{" "}
            <code>providers.registries</code> in your config before enabling the
            integration.
          </p>
        }
      >
        <div class="space-y-4">
          {/* On/off toggle */}
          <div class="flex items-start justify-between gap-4">
            <div class="text-sm text-gray-500 dark:text-gray-400">
              Use a remote knowledge registry as a cache. When enabled, the
              retrieve worker checks the registry before fetching and the{" "}
              <a href="#/remote" class="underline">Remote</a> browse/pull page is
              available for this repository.
            </div>
            <button
              type="button"
              disabled={busyToggle()}
              onClick={toggleEnabled}
              class={`flex-shrink-0 text-xs px-3 py-1.5 rounded border ${
                enabled()
                  ? "bg-green-100 text-green-700 dark:bg-green-900/40 dark:text-green-300 border-green-300 dark:border-green-700"
                  : "bg-gray-100 text-gray-500 dark:bg-gray-700 dark:text-gray-400 border-gray-300 dark:border-gray-600"
              } disabled:opacity-50`}
            >
              {busyToggle() ? "Saving…" : enabled() ? "Enabled" : "Disabled"}
            </button>
          </div>

          {/* Registry selector — only when more than one is configured */}
          <Show when={options().length > 1}>
            <div class="flex items-center justify-between gap-4">
              <label
                for="registry-select"
                class="text-sm text-gray-500 dark:text-gray-400"
              >
                Which registry should this repository use?
              </label>
              <select
                id="registry-select"
                disabled={!enabled() || busySelect()}
                value={currentID()}
                onChange={changeRegistry}
                class="text-sm px-2 py-1.5 rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-700 dark:text-gray-200 disabled:opacity-50"
              >
                <For each={options()}>
                  {(id) => <option value={id}>{id}</option>}
                </For>
              </select>
            </div>
          </Show>

          {/* Auto-contribute (autopush) */}
          <div class="flex items-start justify-between gap-4 pt-3 border-t border-gray-100 dark:border-gray-700">
            <div class="text-sm text-gray-500 dark:text-gray-400">
              <div class="font-medium text-gray-700 dark:text-gray-300">
                Auto-contribute (share back)
              </div>
              <div class="mt-1">
                When enabled, sources are pushed to the remote knowledge registry
                automatically as soon as they finish processing. The retrieved
                sources and facts are shared publicly with other researchers.
                <Show when={!enabled()}>
                  <span class="block mt-1 text-yellow-600 dark:text-yellow-400">
                    Requires the remote registry integration to be enabled.
                  </span>
                </Show>
              </div>
            </div>
            <button
              type="button"
              disabled={!enabled() || busyAuto()}
              onClick={toggleAuto}
              class={`flex-shrink-0 text-xs px-3 py-1.5 rounded border ${
                autoOn()
                  ? "bg-blue-100 text-blue-700 dark:bg-blue-900/40 dark:text-blue-300 border-blue-300 dark:border-blue-700"
                  : "bg-gray-100 text-gray-500 dark:bg-gray-700 dark:text-gray-400 border-gray-300 dark:border-gray-600"
              } disabled:opacity-50`}
            >
              {busyAuto() ? "Saving…" : autoOn() ? "Sharing on" : "Sharing off"}
            </button>
          </div>

          {/* Allowed models (per-repo whitelist) */}
          <div class="flex items-start justify-between gap-4 pt-3 border-t border-gray-100 dark:border-gray-700">
            <div class="text-sm text-gray-500 dark:text-gray-400">
              <div class="font-medium text-gray-700 dark:text-gray-300">
                Allowed cache models
              </div>
              <div class="mt-1">
                Which decomposition models to import from the registry cache.
                {!isPerRepo() && allowedModelsDefault().length > 0
                  ? ` Inheriting global: ${allowedModelsDefault().join(", ")}`
                  : isPerRepo()
                    ? allowedModels().length === 0
                      ? " Per-repo: none (no models imported)"
                      : ` Per-repo: ${allowedModels().join(", ")}`
                    : " Not set (no models imported)."}
              </div>
            </div>
            <div class="flex items-center gap-2">
              <select
                disabled={!enabled() || busyModels()}
                value={""}
                onChange={(e) => {
                  const val = e.currentTarget.value;
                  if (!val) return;
                  e.currentTarget.value = "";
                  const current = isPerRepo() ? [...allowedModels()] : [];
                  if (!current.includes(val)) current.push(val);
                  saveAllowedModels(current);
                }}
                class="text-sm px-2 py-1.5 rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-700 dark:text-gray-200 disabled:opacity-50"
              >
                <option value="">Add model…</option>
                <For each={catalog()}>
                  {(m) => <option value={m.id}>{m.id}</option>}
                </For>
              </select>
            </div>
          </div>
          <Show when={isPerRepo() && allowedModels().length > 0}>
            <div class="flex flex-wrap gap-2 pt-1">
              <For each={allowedModels()}>
                {(m) => (
                  <span class="inline-flex items-center gap-1 text-xs px-2 py-1 rounded bg-gray-100 dark:bg-gray-700 text-gray-600 dark:text-gray-300">
                    {m}
                    <button
                      type="button"
                      disabled={busyModels()}
                      onClick={() => saveAllowedModels(allowedModels().filter((x) => x !== m))}
                      class="text-gray-400 hover:text-red-500 disabled:opacity-50"
                    >
                      ✕
                    </button>
                  </span>
                )}
              </For>
            </div>
          </Show>
          <Show when={isPerRepo()}>
            <button
              type="button"
              disabled={!enabled() || busyModels()}
              onClick={() => saveAllowedModels(null)}
              class="text-xs text-gray-400 hover:text-gray-600 dark:hover:text-gray-300 disabled:opacity-50"
            >
              Reset to global default
            </button>
          </Show>
        </div>
      </Show>
    </Card>
  );
}