import { createMemo, For, Show } from "solid-js";
import Badge from "../../components/Badge";
import Card from "../../components/Card";
import {
  BREAKDOWN_TABS,
  formatCost,
  formatNumber,
  OPERATION_BADGE,
  OPERATION_LABEL,
} from "./constants";

// AIUsageBreakdownTable renders the tabbed breakdown below the
// chart + summary cards. The active tab selects which dataset
// the parent fetched; the parent owns the data + activeTab so the
// tab switch triggers a refetch.
//
// Each tab's rows have a different shape, so we normalize them
// into a common { primary, model, prompt, completion, cost, count }
// row via a per-tab adapter. The table headers change with the
// tab to label the primary column; input/output tokens are always
// shown as two separate right-aligned columns so the cost split
// is visible at a glance.
export default function AIUsageBreakdownTable(props) {
  const headers = createMemo(() => {
    switch (props.activeTab) {
      case "operation":
        return ["Operation", "Model"];
      case "repository":
        return ["Repository", "Model"];
      case "source":
        return ["Source", "Repository"];
      default:
        return ["Provider", "Model"]; // summary rows are per (provider, model, operation)
    }
  });

  const rows = createMemo(() => normalizeRows(props.activeTab, props.rows ?? []));

  return (
    <Card>
      <div class="flex items-center justify-between mb-4 flex-wrap gap-2">
        <h2 class="text-lg font-semibold dark:text-white">Breakdown</h2>
        <nav class="flex gap-1">
          <For each={BREAKDOWN_TABS}>
            {(tab) => (
              <button
                onClick={() => props.onTabChange(tab.id)}
                class={`px-3 py-1 text-xs rounded transition ${
                  props.activeTab === tab.id
                    ? "bg-blue-600 text-white"
                    : "bg-gray-100 dark:bg-gray-700 text-gray-600 dark:text-gray-300 hover:bg-gray-200 dark:hover:bg-gray-600"
                }`}
              >
                {tab.label}
              </button>
            )}
          </For>
        </nav>
      </div>

      <Show
        when={rows().length > 0}
        fallback={
          <p class="text-gray-400 dark:text-gray-500 text-sm py-8 text-center">
            No data for this breakdown.
          </p>
        }
      >
        <div class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="border-b border-gray-200 dark:border-gray-700 text-left">
                <For each={headers()}>
                  {(h) => (
                    <th class="py-2 px-3 font-medium text-gray-500 dark:text-gray-400">{h}</th>
                  )}
                </For>
                <th class="py-2 px-3 font-medium text-gray-500 dark:text-gray-400 text-right">
                  Requests
                </th>
                <th class="py-2 px-3 font-medium text-gray-500 dark:text-gray-400 text-right">
                  Input
                </th>
                <th class="py-2 px-3 font-medium text-gray-500 dark:text-gray-400 text-right">
                  Output
                </th>
                <th class="py-2 px-3 font-medium text-gray-500 dark:text-gray-400 text-right">
                  Est. Cost
                </th>
              </tr>
            </thead>
            <tbody>
              <For each={rows()}>
                {(row) => (
                  <tr class="border-b border-gray-100 dark:border-gray-700/50 hover:bg-gray-50 dark:hover:bg-gray-700/30">
                    <td class="py-2 px-3 font-mono text-xs dark:text-gray-300">
                      {renderPrimary(row)}
                    </td>
                    <td class="py-2 px-3 font-mono text-xs dark:text-gray-300">
                      {row.model || "\u2014"}
                    </td>
                    <td class="py-2 px-3 text-right text-xs text-gray-600 dark:text-gray-400">
                      {formatNumber(row.count)}
                    </td>
                    <td class="py-2 px-3 text-right text-xs text-gray-600 dark:text-gray-400">
                      {formatNumber(row.prompt)}
                    </td>
                    <td class="py-2 px-3 text-right text-xs text-gray-600 dark:text-gray-400">
                      {formatNumber(row.completion)}
                    </td>
                    <td class="py-2 px-3 text-right text-xs text-gray-600 dark:text-gray-400">
                      {formatCost(row.cost)}
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

// renderPrimary renders the primary column cell. For operation
// rows it's a badge; for repository/source rows it's a truncated
// UUID (or "Unattributed" for NULL).
function renderPrimary(row) {
  if (row.primaryKind === "operation") {
    return (
      <Badge variant={OPERATION_BADGE[row.primary] || "gray"}>
        {OPERATION_LABEL[row.primary] || row.primary}
      </Badge>
    );
  }
  if (!row.primary)
    return <span class="text-gray-400 dark:text-gray-500 italic">Unattributed</span>;
  return <span title={row.primary}>{row.primary.slice(0, 8)}</span>;
}

// normalizeRows adapts each tab's row shape to the common table
// row. `primary` is the main grouping key (provider / operation /
// repository / source). prompt + completion are surfaced as two
// separate columns so the cost split stays visible; total is
// omitted from the table (the cards already show the grand total
// and the per-row total is just prompt+completion).
function normalizeRows(tab, rows) {
  switch (tab) {
    case "model":
      return rows.map((r) => ({
        primary: r.provider,
        primaryKind: "text",
        model: `${r.provider} / ${r.model}`,
        count: r.request_count,
        prompt: r.total_prompt_tokens,
        completion: r.total_completion_tokens,
        cost: r.estimated_cost,
      }));
    case "operation":
      return rows.map((r) => ({
        primary: r.operation,
        primaryKind: "operation",
        model: r.model,
        count: r.request_count,
        prompt: r.total_prompt_tokens,
        completion: r.total_completion_tokens,
        cost: r.estimated_cost,
      }));
    case "repository":
      return rows.map((r) => ({
        primary: r.repository_id,
        primaryKind: "uuid",
        model: r.model,
        count: r.request_count,
        prompt: r.total_prompt_tokens,
        completion: r.total_completion_tokens,
        cost: r.estimated_cost,
      }));
    case "source":
      return rows.map((r) => ({
        primary: r.source_id,
        primaryKind: "uuid",
        model: r.model,
        count: r.request_count,
        prompt: r.total_prompt_tokens,
        completion: r.total_completion_tokens,
        cost: r.estimated_cost,
      }));
    default:
      return rows;
  }
}
