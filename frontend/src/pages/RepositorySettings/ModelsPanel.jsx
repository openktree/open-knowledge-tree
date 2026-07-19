import { createSignal, For, Show } from "solid-js";
import Card from "../../components/Card";
import { api } from "../../services/api";

const TASK_KIND_LABELS = {
  fact_extraction: "Fact Extraction",
  image_extraction: "Image Extraction",
  concept_extraction: "Concept Extraction",
  alias_generation: "Alias Generation",
  summarization: "Summarization",
  synthesis: "Synthesis",
};

const TASK_KINDS = [
  "fact_extraction",
  "image_extraction",
  "concept_extraction",
  "alias_generation",
  "summarization",
  "synthesis",
];

export default function ModelsPanel(props) {
  const [busy, setBusy] = createSignal(null);

  const taskModels = () => props.taskModels?.() ?? [];
  const catalog = () => props.catalog?.() ?? [];

  const changeModel = async (kind, modelID) => {
    setBusy(kind);
    try {
      await api.setRepositoryModel(props.repoID(), {
        task_kind: kind,
        model_id: modelID,
      });
      props.onChanged?.();
    } catch (err) {
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setBusy(null);
    }
  };

  return (
    <Card>
      <h3 class="text-lg font-semibold mb-1 dark:text-white">Task Models</h3>
      <p class="text-sm text-gray-500 dark:text-gray-400 mb-4">
        Select which AI model runs each task for this repository. "Default" inherits the global
        config; selecting a model overrides it per-repo.
      </p>
      <div class="space-y-3">
        <For each={taskModels()}>
          {(tm) => {
            const kind = tm.task_kind;
            const label = TASK_KIND_LABELS[kind] || kind;
            const selected = () => tm.selected || "";
            const defaultModel = () => tm.default || "";
            const isDefault = () => !selected();
            return (
              <div class="flex items-center justify-between gap-4">
                <div class="text-sm">
                  <span class="font-medium text-gray-700 dark:text-gray-300">{label}</span>
                  <Show when={isDefault() && defaultModel()}>
                    <span class="ml-2 text-gray-400 dark:text-gray-500">
                      (default: {defaultModel()})
                    </span>
                  </Show>
                </div>
                <select
                  disabled={busy() === kind}
                  value={selected()}
                  onChange={(e) => changeModel(kind, e.currentTarget.value)}
                  class="text-sm px-2 py-1.5 rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-700 dark:text-gray-200 disabled:opacity-50"
                >
                  <option value="">Default{defaultModel() ? ` (${defaultModel()})` : ""}</option>
                  <For each={catalog()}>{(m) => <option value={m.id}>{m.id}</option>}</For>
                </select>
              </div>
            );
          }}
        </For>
        <Show when={taskModels().length === 0}>
          <p class="text-sm text-gray-400 dark:text-gray-500">
            No task model configuration available.
          </p>
        </Show>
      </div>
    </Card>
  );
}
