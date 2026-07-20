import { createSignal, For, Show } from "solid-js";
import Card from "../../components/Card";
import ReportRow from "./ReportRow";
import { buildReportTree, flattenTree } from "./tree";

export default function ReportsTable(props) {
  const reports = () => props.reports() || [];
  const [expandedIds, setExpandedIds] = createSignal(new Set());

  const rows = () => {
    const tree = buildReportTree(reports());
    return flattenTree(tree, expandedIds());
  };

  const toggle = (id) => {
    setExpandedIds((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  return (
    <Card>
      <div class="flex items-center justify-between mb-4">
        <h2 class="text-lg font-semibold dark:text-white">Reports</h2>
        <span class="text-xs text-gray-500 dark:text-gray-400">{reports().length} total</span>
      </div>
      <Show
        when={reports().length > 0}
        fallback={
          <p class="text-sm text-gray-500 dark:text-gray-400">No reports yet. Create one above.</p>
        }
      >
        <div class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="text-left text-xs uppercase text-gray-500 dark:text-gray-400 border-b border-gray-200 dark:border-gray-700">
                <th class="py-2 pr-4">Title</th>
                <th class="py-2 pr-4">Status</th>
                <th class="py-2 pr-4">Sentences</th>
                <th class="py-2 pr-4">Created</th>
                <th class="py-2 pr-4">Actions</th>
              </tr>
            </thead>
            <tbody>
              <For each={rows()}>
                {(row) => (
                  <ReportRow
                    slug={props.slug}
                    report={row.report}
                    depth={row.depth}
                    hasChildren={row.hasChildren}
                    expanded={expandedIds().has(row.report.id)}
                    onToggle={() => toggle(row.report.id)}
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
