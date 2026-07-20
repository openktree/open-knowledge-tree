import { createResource, createSignal, Show } from "solid-js";
import Alert from "../../components/Alert";
import EmptyState from "../../components/EmptyState";
import { api } from "../../services/api";
import AIUsageBreakdownTable from "./AIUsageBreakdownTable";
import AIUsageChart from "./AIUsageChart";
import AIUsageSummaryCards from "./AIUsageSummaryCards";
import { BUCKET_OPTIONS } from "./constants";

// AIUsageContent composes the dashboard. The parent (index.jsx)
// owns the filter signals (from/to/repoID); this component owns
// the tab + bucket state and fetches the four datasets in
// parallel-ish via separate createResource calls. Each resource
// refetches when its inputs change.
//
// The breakdown tab selects which dataset the breakdown table
// shows: "model" → summary rows, "operation" → by-operation rows,
// "repository" → by-repository rows, "source" → by-source rows.
export default function AIUsageContent(props) {
  const [activeTab, setActiveTab] = createSignal("model");
  const [bucket, setBucket] = createSignal("day");

  const filterParams = () => ({
    from: props.from || undefined,
    to: props.to || undefined,
    repository_id: props.repoID || undefined,
  });

  const [summary, { refetch: refetchSummary }] = createResource(filterParams, (p) =>
    api.getAIUsageSummary(p),
  );
  const [byDay, { refetch: refetchByDay }] = createResource(
    () => ({ ...filterParams(), bucket: bucket() }),
    (p) => api.getAIUsageByDay(p),
  );
  const [byOperation] = createResource(filterParams, (p) => api.getAIUsageByOperation(p));
  const [byRepository] = createResource(filterParams, (p) => api.getAIUsageByRepository(p));
  const [bySource] = createResource(filterParams, (p) => api.getAIUsageBySource(p));

  // The breakdown table reads the dataset for the active tab.
  const breakdownRows = () => {
    switch (activeTab()) {
      case "operation":
        return byOperation()?.rows ?? [];
      case "repository":
        return byRepository()?.rows ?? [];
      case "source":
        return bySource()?.rows ?? [];
      default:
        return summary()?.rows ?? [];
    }
  };

  return (
    <div class="space-y-6">
      <Alert
        variant={props.alert()?.variant}
        message={props.alert()?.message}
        onDismiss={() => props.onDismissAlert()}
      />

      <FilterBar
        from={props.from}
        to={props.to}
        repoID={props.repoID}
        bucket={bucket()}
        onFromChange={props.onFromChange}
        onToChange={props.onToChange}
        onRepoIDChange={props.onRepoIDChange}
        onBucketChange={setBucket}
        onRefresh={() => {
          refetchSummary();
          refetchByDay();
        }}
      />

      <Show when={!summary.loading} fallback={<EmptyState title="Loading usage data…" />}>
        <AIUsageSummaryCards summary={summary()} />
      </Show>

      <Show when={!byDay.loading} fallback={<EmptyState title="Loading chart…" />}>
        <AIUsageChart rows={byDay()?.rows ?? []} bucket={bucket()} />
      </Show>

      <AIUsageBreakdownTable
        activeTab={activeTab()}
        rows={breakdownRows()}
        onTabChange={setActiveTab}
      />
    </div>
  );
}

// FilterBar is the inline filter row. from/to are RFC3339 datetime
// inputs (the user types a date; we send it through). repoID is
// a free-text UUID filter. bucket selects the chart width.
function FilterBar(props) {
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
        Repository ID
        <input
          type="text"
          placeholder="UUID (optional)"
          value={props.repoID || ""}
          onChange={(e) => props.onRepoIDChange(e.currentTarget.value)}
          class="mt-1 px-2 py-1 rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-sm dark:text-white font-mono"
        />
      </label>
      <label class="flex flex-col text-xs text-gray-500 dark:text-gray-400">
        Bucket
        <select
          value={props.bucket}
          onChange={(e) => props.onBucketChange(e.currentTarget.value)}
          class="mt-1 px-2 py-1 rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-sm dark:text-white"
        >
          {BUCKET_OPTIONS.map((o) => (
            <option value={o.value}>{o.label}</option>
          ))}
        </select>
      </label>
      <button
        onClick={props.onRefresh}
        class="ml-auto px-3 py-1 text-sm rounded bg-blue-600 text-white hover:bg-blue-700"
      >
        Refresh
      </button>
    </div>
  );
}
