import { For, Show } from "solid-js";
import { BUILTIN_SOURCE } from "./constants";

// PromptsetsTable renders the list of promptsets (built-in + custom)
// with actions. The built-in is non-editable; custom rows carry
// Edit + Delete buttons. Controlled by the parent.
//
// Props:
//   promptsets  – accessor []Promptset
//   onEdit      – (ps) => void  open the edit form for a custom ps
//   onDelete    – (ps) => void  delete a custom ps
//   busyHash    – accessor string  hash currently being mutated
export default function PromptsetsTable(props) {
  const rows = () => props.promptsets?.() ?? [];
  const busy = () => props.busyHash?.() ?? "";

  return (
    <div class="overflow-x-auto">
      <table class="min-w-full text-sm">
        <thead>
          <tr class="text-left text-gray-500 dark:text-gray-400 border-b border-gray-200 dark:border-gray-700">
            <th class="py-2 pr-4 font-medium">Name</th>
            <th class="py-2 pr-4 font-medium">Source</th>
            <th class="py-2 pr-4 font-medium">Hash</th>
            <th class="py-2 pr-4 font-medium text-right">Actions</th>
          </tr>
        </thead>
        <tbody>
          <For each={rows()}>
            {(ps) => (
              <tr class="border-b border-gray-100 dark:border-gray-800">
                <td class="py-2 pr-4 text-gray-700 dark:text-gray-300">
                  {ps.name}
                </td>
                <td class="py-2 pr-4">
                  <Show when={ps.source === BUILTIN_SOURCE}>
                    <span class="px-2 py-0.5 text-xs rounded bg-gray-100 dark:bg-gray-700 text-gray-600 dark:text-gray-300">
                      built-in
                    </span>
                  </Show>
                  <Show when={ps.source !== BUILTIN_SOURCE}>
                    <span class="px-2 py-0.5 text-xs rounded bg-blue-100 dark:bg-blue-900 text-blue-700 dark:text-blue-200">
                      custom
                    </span>
                  </Show>
                </td>
                <td class="py-2 pr-4 font-mono text-xs text-gray-500 dark:text-gray-400">
                  {ps.hash ? ps.hash.slice(0, 12) + "…" : ""}
                </td>
                <td class="py-2 pr-4 text-right">
                  <Show when={ps.source !== BUILTIN_SOURCE}>
                    <button
                      type="button"
                      onClick={() => props.onEdit?.(ps)}
                      disabled={busy() === ps.hash}
                      class="px-2 py-1 text-xs rounded border border-gray-300 dark:border-gray-600 text-gray-700 dark:text-gray-300 hover:bg-gray-50 dark:hover:bg-gray-700 disabled:opacity-50 mr-2"
                    >
                      Edit
                    </button>
                    <button
                      type="button"
                      onClick={() => props.onDelete?.(ps)}
                      disabled={busy() === ps.hash}
                      class="px-2 py-1 text-xs rounded border border-red-300 dark:border-red-700 text-red-700 dark:text-red-300 hover:bg-red-50 dark:hover:bg-red-900 disabled:opacity-50"
                    >
                      Delete
                    </button>
                  </Show>
                </td>
              </tr>
            )}
          </For>
        </tbody>
      </table>
    </div>
  );
}