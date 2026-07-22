import { createResource, createSignal, Show } from "solid-js";
import Alert from "../../components/Alert";
import EmptyState from "../../components/EmptyState";
import { api } from "../../services/api";
import AIUsageBreakdownTable from "../AIUsage/AIUsageBreakdownTable";
import AIUsageChart from "../AIUsage/AIUsageChart";
import AIUsageSummaryCards from "../AIUsage/AIUsageSummaryCards";
import { BUCKET_OPTIONS } from "../AIUsage/constants";

// RepoAIUsageContent composes the per-repo AI usage dashboard.
// The parent (index.jsx) owns the from/to filter signals + alert;
// this component owns the tab + bucket state and fetches the four
// datasets via the repo-scoped api.getRepoAIUsage* methods. Each
// resource refetches when its inputs change.
//
// The breakdown tab selects which dataset the breakdown table
// shows: "model" → summary, "operation" → by-operation,
// "repository" → by-repository (degenerate — one row group — but
// kept for UI parity), "source" → by-source. The "Repository ID"
// filter from the system page is dropped here — the server forces
// the repository_id from the URL context.
export default function RepoAIUsageContent(props) {
  const [activeTab, setActiveTab] = createSignal("model");
  const [bucket, setBucket] = createSignal("day");

  const filterParams = () => ({
    from: props.from || undefined,
    to: props.to || undefined,
  });

  const [summary, { refetch: refetchSummary }] = createResource(filterParams, (p) =>
    api.getRepoAIUsageSummary(props.slug, p),
  );
  const [byDay, { refetch: refetchByDay }] = createResource(
    () => ({ ...filterParams(), bucket: bucket() }),
    (p) => api.getRepoAIUsageByDay(props.slug, p),
  );
  const [byOperation] = createResource(filterParams, (p) =>
    api.getRepoAIUsageByOperation(props.slug, p),
  );
  const [byRepository] = createResource(filterParams, (p) =>
    api.getRepoAIUsageByRepository(props.slug, p),
  );
  const [bySource] = createResource(filterParams, (p) => api.getRepoAIUsageBySource(props.slug, p));

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
        bucket={bucket()}
        onFromChange={props.onFromChange}
        onToChange={props.onToChange}
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
// inputs. bucket selects the chart width. The "Repository ID"
// filter from the system page is intentionally absent — the
// server forces the repository_id from the URL context.
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
