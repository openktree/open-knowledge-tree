import { For, Show } from "solid-js";
import Badge from "../Badge";
import Card from "../Card";
import { ACTION_BADGE, ACTION_LABEL, formatDetail, formatTime } from "./constants";

// AuditTable renders the audit events list. It is shared by the
// system Audit page (/audit) and the per-repo Audit tab — both
// feed it the same row shape from api.listSystemAudit /
// api.listRepoAudit. The parent owns the data + pagination
// signals; this component is purely controlled.
//
// Each row shows: occurred_at, action (badge), object, actor
// (email + truncated user_id), target, and an expandable detail
// JSONB dump. repository_id is shown only when present (system
// view); the repo view scopes server-side so the column is
// redundant there.
export default function AuditTable(props) {
  const events = () => props.events ?? [];
  const showRepo = () => props.showRepo !== false; // default true

  return (
    <Card>
      <Show
        when={events().length > 0}
        fallback={
          <p class="text-gray-400 dark:text-gray-500 text-sm py-8 text-center">
            No audit events match the current filters.
          </p>
        }
      >
        <div class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="border-b border-gray-200 dark:border-gray-700 text-left">
                <th class="py-2 px-3 font-medium text-gray-500 dark:text-gray-400">Time</th>
                <th class="py-2 px-3 font-medium text-gray-500 dark:text-gray-400">Action</th>
                <th class="py-2 px-3 font-medium text-gray-500 dark:text-gray-400">Object</th>
                <th class="py-2 px-3 font-medium text-gray-500 dark:text-gray-400">Actor</th>
                <th class="py-2 px-3 font-medium text-gray-500 dark:text-gray-400">Target</th>
                <Show when={showRepo()}>
                  <th class="py-2 px-3 font-medium text-gray-500 dark:text-gray-400">Repo</th>
                </Show>
                <th class="py-2 px-3 font-medium text-gray-500 dark:text-gray-400">Detail</th>
              </tr>
            </thead>
            <tbody>
              <For each={events()}>
                {(ev) => (
                  <tr class="border-b border-gray-100 dark:border-gray-700/50 hover:bg-gray-50 dark:hover:bg-gray-700/30 align-top">
                    <td class="py-2 px-3 text-xs text-gray-600 dark:text-gray-400 whitespace-nowrap">
                      {formatTime(ev.occurred_at)}
                    </td>
                    <td class="py-2 px-3">
                      <Badge variant={ACTION_BADGE[ev.action] || "gray"}>
                        {ACTION_LABEL[ev.action] || ev.action}
                      </Badge>
                    </td>
                    <td class="py-2 px-3 font-mono text-xs dark:text-gray-300">{ev.object}</td>
                    <td class="py-2 px-3 text-xs dark:text-gray-300">
                      <div class="flex flex-col gap-0.5">
                        <span class="text-gray-700 dark:text-gray-200">
                          {ev.actor_email || ev.actor_username || "\u2014"}
                        </span>
                        <Show when={ev.actor_user_id}>
                          <span
                            class="font-mono text-[10px] text-gray-400"
                            title={ev.actor_user_id}
                          >
                            {ev.actor_user_id?.slice(0, 8)}
                          </span>
                        </Show>
                      </div>
                    </td>
                    <td class="py-2 px-3 font-mono text-xs dark:text-gray-300 max-w-xs truncate">
                      <span title={ev.target || ""}>{ev.target || "\u2014"}</span>
                    </td>
                    <Show when={showRepo()}>
                      <td class="py-2 px-3 font-mono text-xs dark:text-gray-300">
                        <Show
                          when={ev.repository_id}
                          fallback={<span class="text-gray-400 italic">system</span>}
                        >
                          <span title={ev.repository_id}>{ev.repository_id?.slice(0, 8)}</span>
                        </Show>
                      </td>
                    </Show>
                    <td class="py-2 px-3 text-xs dark:text-gray-300">
                      <DetailToggle detail={ev.detail} sourceUrl={ev.source_url} />
                    </td>
                  </tr>
                )}
              </For>
            </tbody>
          </table>
        </div>
      </Show>
    </Card>
  );
}

// DetailToggle expands/collapses the JSONB detail column. The
// detail is rendered as a <pre> block with the formatted JSON;
// source_url (present on ingestion_start rows) is shown above the
// JSON when present so the row links back to the fetched URL.
function DetailToggle(props) {
  let open = false;
  const toggle = (e) => {
    open = !open;
    const el = e.currentTarget.nextElementSibling;
    if (el) el.style.display = open ? "block" : "none";
  };
  return (
    <div>
      <button onClick={toggle} class="text-blue-600 dark:text-blue-400 hover:underline text-xs">
        view
      </button>
      <div style={{ display: "none" }} class="mt-1 max-w-md">
        <Show when={props.sourceUrl}>
          <div class="font-mono text-[10px] text-gray-500 dark:text-gray-400 break-all mb-1">
            <a
              href={props.sourceUrl}
              target="_blank"
              rel="noreferrer noopener"
              class="hover:underline"
            >
              {props.sourceUrl}
            </a>
          </div>
        </Show>
        <pre class="text-[10px] text-gray-600 dark:text-gray-300 bg-gray-50 dark:bg-gray-800 rounded p-2 overflow-x-auto">
          {formatDetail(props.detail)}
        </pre>
      </div>
    </div>
  );
}
