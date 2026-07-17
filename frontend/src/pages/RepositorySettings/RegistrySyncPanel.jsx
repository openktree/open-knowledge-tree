import { createSignal, Show } from "solid-js";
import { api } from "../../services/api";
import Card from "../../components/Card";

const LEVEL_OPTIONS = [
  { value: "facts", label: "Facts only" },
  { value: "concepts", label: "Facts + Concepts" },
];

export default function RegistrySyncPanel(props) {
  const [busyContribute, setBusyContribute] = createSignal(false);
  const [busyPull, setBusyPull] = createSignal(false);
  const [busyLevels, setBusyLevels] = createSignal(false);
  const [result, setResult] = createSignal(null);
  const [pushLevel, setPushLevel] = createSignal("");
  const [pullLevel, setPullLevel] = createSignal("");

  // Sync local selects from the server-provided levels. createSignal
  // initializes empty; this effect runs when the resource loads.
  const serverPush = () => props.pushLevel?.() || "concepts";
  const serverPull = () => props.pullLevel?.() || "concepts";
  if (!pushLevel()) setPushLevel(serverPush());
  if (!pullLevel()) setPullLevel(serverPull());

  const handleContributeAll = async () => {
    setBusyContribute(true);
    setResult(null);
    try {
      const res = await api.contributeAll(props.repoID());
      setResult({ variant: "success", message: `Contribute All enqueued (job: ${res.job_id})` });
    } catch (err) {
      setResult({ variant: "error", message: err.message });
    } finally {
      setBusyContribute(false);
    }
  };

  const handlePullAll = async () => {
    setBusyPull(true);
    setResult(null);
    try {
      const res = await api.pullAllFromRegistry(props.repoID());
      setResult({ variant: "success", message: `Pull All enqueued (job: ${res.job_id})` });
    } catch (err) {
      setResult({ variant: "error", message: err.message });
    } finally {
      setBusyPull(false);
    }
  };

  const handleSaveLevels = async () => {
    setBusyLevels(true);
    setResult(null);
    try {
      await api.setRepositorySyncLevels(props.repoID(), {
        push_level: pushLevel(),
        pull_level: pullLevel(),
      });
      setResult({ variant: "success", message: "Sync levels saved." });
      props.onChanged?.();
    } catch (err) {
      setResult({ variant: "error", message: err.message });
    } finally {
      setBusyLevels(false);
    }
  };

  const configured = () => props.registryConfigured?.() !== false;
  const enabled = () => props.registryEnabled?.() !== false;
  const canAct = () => configured() && enabled();
  const levelsDirty = () => pushLevel() !== serverPush() || pullLevel() !== serverPull();

  return (
    <Card>
      <h3 class="text-lg font-semibold mb-3 dark:text-white">Registry Sync</h3>
      <Show
        when={configured()}
        fallback={
          <p class="text-sm text-yellow-600 dark:text-yellow-400">
            Registry is not configured. Set <code>providers.registry.url</code> or <code>providers.registry.registries</code> in your config.
          </p>
        }
      >
        <Show
          when={enabled()}
          fallback={
            <p class="text-sm text-yellow-600 dark:text-yellow-400">
              The remote registry integration is disabled for this repository.
              Enable it in the panel above to push and pull from the registry.
            </p>
          }
        >
          <p class="text-sm text-gray-500 dark:text-gray-400 mb-4">
            Push all processed sources to the registry, or pull available
            decompositions from it. Each operation runs as a background job.
          </p>

          <div class="mb-4 p-3 border rounded dark:border-gray-700 bg-gray-50 dark:bg-gray-800/50">
            <h4 class="text-sm font-semibold mb-2 dark:text-gray-200">Sync Levels</h4>
            <p class="text-xs text-gray-500 dark:text-gray-400 mb-3">
              Controls how much data is included when pushing to or pulling from
              the registry. <strong>Facts only</strong> = sources + facts + fact
              embeddings (concepts regenerated locally on pull). <strong>Facts +
              Concepts</strong> = adds concepts, links, and concept embeddings
              (the default, full sync).
            </p>
            <div class="flex flex-wrap items-end gap-4">
              <label class="text-sm">
                <span class="block text-gray-600 dark:text-gray-300 mb-1">Push level</span>
                <select
                  value={pushLevel()}
                  onInput={(e) => setPushLevel(e.currentTarget.value)}
                  class="px-2 py-1 rounded border dark:bg-gray-900 dark:border-gray-600 dark:text-white"
                >
                  {LEVEL_OPTIONS.map((o) => <option value={o.value}>{o.label}</option>)}
                </select>
              </label>
              <label class="text-sm">
                <span class="block text-gray-600 dark:text-gray-300 mb-1">Pull level</span>
                <select
                  value={pullLevel()}
                  onInput={(e) => setPullLevel(e.currentTarget.value)}
                  class="px-2 py-1 rounded border dark:bg-gray-900 dark:border-gray-600 dark:text-white"
                >
                  {LEVEL_OPTIONS.map((o) => <option value={o.value}>{o.label}</option>)}
                </select>
              </label>
              <button
                type="button"
                disabled={!levelsDirty() || busyLevels()}
                onClick={handleSaveLevels}
                class="text-sm px-3 py-1.5 rounded border bg-gray-100 text-gray-700 dark:bg-gray-700 dark:text-gray-200 border-gray-300 dark:border-gray-600 hover:bg-gray-200 dark:hover:bg-gray-600 disabled:opacity-50"
              >
                {busyLevels() ? "Saving…" : "Save Levels"}
              </button>
            </div>
          </div>
        </Show>
      </Show>

      <Show when={result()}>
        {(r) => (
          <div
            class={`mb-3 text-sm px-3 py-2 rounded ${
              r().variant === "error"
                ? "bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300"
                : "bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-300"
            }`}
          >
            {r().message}
          </div>
        )}
      </Show>

      <div class="flex gap-3">
        <button
          type="button"
          disabled={!canAct() || busyContribute()}
          onClick={handleContributeAll}
          class="text-sm px-4 py-2 rounded border bg-blue-100 text-blue-700 dark:bg-blue-900/40 dark:text-blue-300 border-blue-300 dark:border-blue-700 hover:bg-blue-200 dark:hover:bg-blue-900/60 disabled:opacity-50"
        >
          {busyContribute() ? "Enqueuing…" : "Push All to Registry"}
        </button>
        <button
          type="button"
          disabled={!canAct() || busyPull()}
          onClick={handlePullAll}
          class="text-sm px-4 py-2 rounded border bg-purple-100 text-purple-700 dark:bg-purple-900/40 dark:text-purple-300 border-purple-300 dark:border-purple-700 hover:bg-purple-200 dark:hover:bg-purple-900/60 disabled:opacity-50"
        >
          {busyPull() ? "Enqueuing…" : "Pull All from Registry"}
        </button>
      </div>
    </Card>
  );
}