import { Show } from "solid-js";
import Button from "../../components/Button";
import { formatTimestamp } from "./constants";

// RemoteRow renders a single source from the remote registry
// list. The title area is a button that opens the
// RemoteDetailDialog (via props.onOpenDetail); the Pull button
// on the right triggers the import path. The previous
// inline expand/collapse of the raw JSON row is gone —
// that's now part of the dialog (under "Show source JSON").
export default function RemoteRow(props) {
  const source = () => props.source;

  return (
    <div class="border border-gray-200 dark:border-gray-700 rounded bg-white dark:bg-gray-800">
      <div class="p-3 flex items-start justify-between gap-4">
        <button
          type="button"
          onClick={() => props.onOpenDetail(source())}
          class="flex-1 min-w-0 text-left"
        >
          <div class="font-medium text-sm text-gray-900 dark:text-white truncate">
            {source().title || source().url || "(no title)"}
          </div>
          <Show when={source().url}>
            <div class="text-xs text-gray-500 dark:text-gray-400 truncate mt-0.5">
              {source().url}
            </div>
          </Show>
          <div class="flex items-center gap-3 mt-1 text-xs text-gray-400 dark:text-gray-500">
            <Show when={source().doi}>
              <span>DOI: {source().doi}</span>
            </Show>
            <span>{formatTimestamp(source().created_at)}</span>
            <Show when={props.exists}>
              <span class="text-green-600 dark:text-green-400 font-medium">Already imported</span>
            </Show>
          </div>
        </button>
        <Button
          variant={props.exists ? "secondary" : "primary"}
          onClick={() => props.onPull(source())}
          loading={props.pullingID() === source().id}
          loadingText={props.exists ? "Re-syncing..." : "Pulling..."}
          class="text-xs shrink-0"
        >
          {props.exists ? "Re-sync" : "Pull"}
        </Button>
      </div>
    </div>
  );
}
