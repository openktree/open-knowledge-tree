import { createMemo, createResource, For, Show } from "solid-js";
import { computeRegistryHash, PHASE_KEYS, PHASE_LABELS, REGISTRY_SHARED_KEYS } from "./constants";

// PromptsetForm renders the 8 phase textareas + the name field for
// create/edit. Controlled: the parent owns the draft state and
// calls back via onChange. The form is deliberately dumb —
// validation + submit live in the parent so the same form serves
// both create and edit.
//
// A live "Registry compatibility" preview recomputes the
// registry hash (over only the 4 shared phases) as the user edits
// and shows whether the current draft is compatible with the
// built-in philosophy — so the user can see "this will be
// compatible with: Built-in" before saving and understand that
// tweaking the summarizer alone does not fracture the registry
// graph.
//
// Props:
//   draft           – accessor returning the current draft {name, ...phases}
//   onChange        – (field, value) => void  updates one field
//   onSubmit        – () => void               save
//   onCancel        – () => void               discard
//   busy            – accessor bool            disable inputs while saving
//   submitLabel     – string                   button label ("Create" / "Save")
//   defaultRegistryHash – accessor string      the built-in's registry_hash
//                                              (used to badge "≡ default")
export default function PromptsetForm(props) {
  const draft = () => props.draft() || {};
  const name = () => draft().name || "";
  const phase = (key) => draft()[key] || "";

  // Live preview of the registry hash. Recomputes whenever any of
  // the 4 shared phase fields changes. createResource handles the
  // async crypto.subtle.digest call.
  const sharedFingerprint = createMemo(() =>
    REGISTRY_SHARED_KEYS.map((k) => phase(k)).join("\u0000"),
  );
  const [registryHash] = createResource(sharedFingerprint, computeRegistryHash);
  const defaultHash = () => props.defaultRegistryHash?.() ?? "";
  const isDefaultCompatible = () =>
    !!registryHash() && !!defaultHash() && registryHash() === defaultHash();

  return (
    <div class="space-y-4">
      <div>
        <label class="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">Name</label>
        <input
          type="text"
          value={name()}
          onInput={(e) => props.onChange?.("name", e.currentTarget.value)}
          disabled={props.busy?.()}
          placeholder="e.g. My Custom Philosophy"
          class="w-full text-sm px-3 py-2 rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-700 dark:text-gray-200 disabled:opacity-50"
        />
      </div>
      <div class="rounded border border-gray-200 dark:border-gray-700 bg-gray-50 dark:bg-gray-800 px-3 py-2 text-xs">
        <div class="flex items-center gap-2 flex-wrap">
          <span class="font-medium text-gray-600 dark:text-gray-300">Registry compatibility:</span>
          <span class="font-mono text-gray-500 dark:text-gray-400">
            {registryHash() ? registryHash().slice(0, 16) + "…" : "…"}
          </span>
          <Show when={isDefaultCompatible()}>
            <span
              class="px-1.5 py-0.5 text-xs rounded bg-green-100 dark:bg-green-900 text-green-700 dark:text-green-200"
              title="Shares the 4 registry-shared phases with the built-in — decompositions are exchangeable via the registry."
            >
              ≡ default
            </span>
          </Show>
        </div>
        <p class="text-gray-400 dark:text-gray-500 mt-1">
          Computed over only the 4 shared phases (fact, image-fact, concept, refinement). Editing
          the local-only phases (synthesis, image-picker, summarization, posture) does not change it
          — those phases run locally and their output is not pushed to the registry.
        </p>
      </div>
      <For each={PHASE_KEYS}>
        {(key) => (
          <div>
            <label class="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">
              {PHASE_LABELS[key]}
              <Show when={REGISTRY_SHARED_KEYS.includes(key)}>
                <span
                  class="ml-1 text-xs text-gray-400 dark:text-gray-500"
                  title="This phase feeds the registry-compatibility hash."
                >
                  ·shared
                </span>
              </Show>
            </label>
            <textarea
              value={phase(key)}
              onInput={(e) => props.onChange?.(key, e.currentTarget.value)}
              disabled={props.busy?.()}
              rows={6}
              class="w-full text-sm px-3 py-2 rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-700 dark:text-gray-200 font-mono disabled:opacity-50"
            />
          </div>
        )}
      </For>
      <div class="flex gap-2 justify-end">
        <Show when={props.onCancel}>
          <button
            type="button"
            onClick={props.onCancel}
            disabled={props.busy?.()}
            class="px-3 py-1.5 text-sm rounded border border-gray-300 dark:border-gray-600 text-gray-700 dark:text-gray-300 hover:bg-gray-50 dark:hover:bg-gray-700 disabled:opacity-50"
          >
            Cancel
          </button>
        </Show>
        <button
          type="button"
          onClick={props.onSubmit}
          disabled={props.busy?.()}
          class="px-3 py-1.5 text-sm rounded bg-blue-600 text-white hover:bg-blue-700 disabled:opacity-50"
        >
          {props.submitLabel || "Save"}
        </button>
      </div>
    </div>
  );
}
