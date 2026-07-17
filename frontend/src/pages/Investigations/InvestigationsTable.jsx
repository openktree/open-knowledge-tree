import { Show, For } from "solid-js";
import { A } from "@solidjs/router";
import Button from "../../components/Button";
import Card from "../../components/Card";
import InvestigationRow from "./InvestigationRow";

export default function InvestigationsTable(props) {
  const invs = () => props.investigations() || [];
  return (
    <Card>
      <div class="flex items-center justify-between mb-4">
        <h2 class="text-lg font-semibold dark:text-white">Investigations</h2>
        <span class="text-xs text-gray-500 dark:text-gray-400">
          {invs().length} total
        </span>
      </div>
      <Show
        when={invs().length > 0}
        fallback={
          <p class="text-sm text-gray-500 dark:text-gray-400">
            Nothing here yet. Use the form above to create your first investigation.
          </p>
        }
      >
        <div class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="text-left border-b dark:border-gray-700">
                <th class="py-3 px-4 font-medium text-gray-600 dark:text-gray-400">Title</th>
                <th class="py-3 px-4 font-medium text-gray-600 dark:text-gray-400">Topic</th>
                <th class="py-3 px-4 font-medium text-gray-600 dark:text-gray-400">Created</th>
                <th class="py-3 px-4 font-medium text-gray-600 dark:text-gray-400 text-right">Actions</th>
              </tr>
            </thead>
            <tbody>
              <For each={invs()}>
                {(inv) => (
                  <InvestigationRow
                    slug={props.slug}
                    inv={inv}
                    onUpdated={props.onUpdated}
                    onDeleted={props.onDeleted}
                    onAlert={props.onAlert}
                  />
                )}
              </For>
            </tbody>
          </table>
        </div>
      </Show>
    </Card>
  );
}