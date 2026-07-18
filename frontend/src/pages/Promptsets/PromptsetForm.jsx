import { createSignal, For, Show } from "solid-js";
import { PHASE_LABELS, PHASE_KEYS } from "./constants";

// PromptsetForm renders the 8 phase textareas + the name field for
// create/edit. Controlled: the parent owns the draft state and
// calls back via onChange. The form is deliberately dumb —
// validation + submit live in the parent so the same form serves
// both create and edit.
//
// Props:
//   draft     – accessor returning the current draft {name, ...phases}
//   onChange  – (field, value) => void  updates one field
//   onSubmit  – () => void               save
//   onCancel  – () => void               discard
//   busy      – accessor bool            disable inputs while saving
//   submitLabel – string                 button label ("Create" / "Save")
export default function PromptsetForm(props) {
  const draft = () => props.draft() || {};
  const name = () => draft().name || "";
  const phase = (key) => draft()[key] || "";

  return (
    <div class="space-y-4">
      <div>
        <label class="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">
          Name
        </label>
        <input
          type="text"
          value={name()}
          onInput={(e) => props.onChange?.("name", e.currentTarget.value)}
          disabled={props.busy?.()}
          placeholder="e.g. My Custom Philosophy"
          class="w-full text-sm px-3 py-2 rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-700 dark:text-gray-200 disabled:opacity-50"
        />
      </div>
      <For each={PHASE_KEYS}>
        {(key) => (
          <div>
            <label class="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">
              {PHASE_LABELS[key]}
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