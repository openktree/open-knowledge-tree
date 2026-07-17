import { createResource, For, Show, createMemo } from "solid-js";
import { api } from "../../services/api";
import FormField from "../../components/FormField";

// PresetPicker renders the "Repository type" dropdown populated from
// GET /repositories/presets, plus an advanced disclosure that
// reveals provider toggles + a context multi-select for overrides.
// When "Custom" is picked the advanced panel expands automatically.
//
// Props:
//   - value:    () => {preset, providers, contexts}
//   - onChange: (next) => void
export default function PresetPicker(props) {
  const [presets] = createResource(() => api.listRepositoryPresets().catch(() => null));
  const list = () => presets()?.presets || [];
  const defaultPreset = () => presets()?.default_preset || "";

  const current = () => props.value() || { preset: "", providers: {}, contexts: [] };
  const set = (patch) => props.onChange?.({ ...current(), ...patch });

  const selectedPreset = createMemo(() => list().find((p) => p.id === current().preset));
  const allLiveProviders = createMemo(() => {
    // The preset list doesn't carry the live catalog; the settings
    // page does. For the create form we surface the preset's
    // declared provider ids; an override is a free-form list.
    const out = { search: [], resolution: [] };
    for (const p of list()) {
      for (const k of ["search", "resolution"]) {
        for (const id of p.providers?.[k] || []) {
          if (!out[k].includes(id)) out[k].push(id);
        }
      }
    }
    return out;
  });

  return (
    <div class="space-y-3">
      <FormField
        label="Repository type"
        type="select"
        name="preset"
        value={current().preset || defaultPreset()}
        onChange={(v) => set({ preset: v })}
      >
        <option value="">Default ({defaultPreset() || "general"})</option>
        <For each={list()}>
          {(p) => <option value={p.id}>{p.label}{p.id === defaultPreset() ? " (default)" : ""}</option>}
        </For>
      </FormField>
      <Show when={selectedPreset()?.description}>
        <p class="text-xs text-gray-500 dark:text-gray-400">{selectedPreset().description}</p>
      </Show>
      <details class="text-sm">
        <summary class="cursor-pointer text-blue-600 dark:text-blue-400">Advanced (override providers / contexts)</summary>
        <div class="mt-3 space-y-3">
          <div>
            <p class="text-xs font-medium mb-1 dark:text-gray-300">Search providers</p>
            <div class="flex flex-wrap gap-2">
              <For each={allLiveProviders().search}>
                {(id) => (
                  <label class="text-xs flex items-center gap-1">
                    <input
                      type="checkbox"
                      checked={(current().providers.search || []).includes(id)}
                      onChange={(e) => {
                        const cur = current().providers.search || [];
                        const next = e.target.checked ? [...cur, id] : cur.filter((x) => x !== id);
                        set({ providers: { ...current().providers, search: next } });
                      }}
                    />
                    {id}
                  </label>
                )}
              </For>
            </div>
          </div>
          <div>
            <p class="text-xs font-medium mb-1 dark:text-gray-300">Resolution providers</p>
            <div class="flex flex-wrap gap-2">
              <For each={allLiveProviders().resolution}>
                {(id) => (
                  <label class="text-xs flex items-center gap-1">
                    <input
                      type="checkbox"
                      checked={(current().providers.resolution || []).includes(id)}
                      onChange={(e) => {
                        const cur = current().providers.resolution || [];
                        const next = e.target.checked ? [...cur, id] : cur.filter((x) => x !== id);
                        set({ providers: { ...current().providers, resolution: next } });
                      }}
                    />
                    {id}
                  </label>
                )}
              </For>
            </div>
          </div>
          <p class="text-xs text-gray-500 dark:text-gray-400">
            Leave providers unchecked to inherit the preset's set. An explicit
            selection overrides the preset for that kind.
          </p>
        </div>
      </details>
    </div>
  );
}