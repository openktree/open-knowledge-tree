import { Show } from "solid-js";

// AuditFilterBar is the inline filter row shared by the system
// Audit page and the per-repo Audit tab. from / to are RFC3339
// date inputs; action is a dropdown populated from the server's
// distinct actions list (passed in by the parent); actor_user_id
// is a free-text UUID filter. The parent owns the filter signals;
// this component is purely controlled.
//
// Props:
//   from, to, action, actorUserID — current filter values
//   actions — string[] of distinct action names (for the dropdown)
//   onFromChange, onToChange, onActionChange, onActorUserIDChange
//   onRefresh — re-fetch
//   showActorFilter — defaults true; the repo tab can hide it
//     (repo-scoped audit rarely needs an actor filter)
export default function AuditFilterBar(props) {
  return (
    <div class="bg-white dark:bg-gray-800 rounded-lg shadow-md p-4 flex flex-wrap items-end gap-4">
      <label class="flex flex-col text-xs text-gray-500 dark:text-gray-400">
        From
        <input
          type="date"
          value={props.from?.slice(0, 10) || ""}
          onChange={(e) =>
            props.onFromChange(
              e.currentTarget.value ? new Date(e.currentTarget.value).toISOString() : "",
            )
          }
          class="mt-1 px-2 py-1 rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-sm dark:text-white"
        />
      </label>
      <label class="flex flex-col text-xs text-gray-500 dark:text-gray-400">
        To
        <input
          type="date"
          value={props.to?.slice(0, 10) || ""}
          onChange={(e) =>
            props.onToChange(
              e.currentTarget.value ? new Date(e.currentTarget.value).toISOString() : "",
            )
          }
          class="mt-1 px-2 py-1 rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-sm dark:text-white"
        />
      </label>
      <label class="flex flex-col text-xs text-gray-500 dark:text-gray-400">
        Action
        <select
          value={props.action || ""}
          onChange={(e) => props.onActionChange(e.currentTarget.value)}
          class="mt-1 px-2 py-1 rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-sm dark:text-white"
        >
          <option value="">All actions</option>
          {(props.actions ?? []).map((a) => (
            <option value={a}>{a}</option>
          ))}
        </select>
      </label>
      <Show when={props.showActorFilter !== false}>
        <label class="flex flex-col text-xs text-gray-500 dark:text-gray-400">
          Actor user ID
          <input
            type="text"
            placeholder="UUID (optional)"
            value={props.actorUserID || ""}
            onChange={(e) => props.onActorUserIDChange(e.currentTarget.value)}
            class="mt-1 px-2 py-1 rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-sm dark:text-white font-mono"
          />
        </label>
      </Show>
      <button
        onClick={props.onRefresh}
        class="ml-auto px-3 py-1 text-sm rounded bg-blue-600 text-white hover:bg-blue-700"
      >
        Refresh
      </button>
    </div>
  );
}
