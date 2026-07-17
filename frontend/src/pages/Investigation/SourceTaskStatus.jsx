import { createResource, createSignal, Show, For, createEffect } from "solid-js";
import { api } from "../../services/api";
import Badge from "../../components/Badge";
import Button from "../../components/Button";
import { STATE_BADGE } from "../Tasks/constants";
import JobDetail from "../Tasks/JobDetail";

const RELEVANT_KINDS = [
  "retrieve_source",
  "source_decomposition",
  "embed_facts",
  "extract_concepts",
  "embed_concepts",
  "cleanup_facts",
];
const RUNNING_STATES = ["available", "running", "retryable", "scheduled", "pending"];

export default function SourceTaskStatus(props) {
  const [refreshKey, setRefreshKey] = createSignal(0);
  const [expandedJob, setExpandedJob] = createSignal(null);

  const [tasksData, { refetch }] = createResource(
    () => [props.slug, props.sourceID, refreshKey()],
    async ([s, id]) => {
      if (!s || !id) return { jobs: [] };
      try {
        return await api.listRepoTasks(s, { source_id: id, limit: 20 });
      } catch {
        return { jobs: [] };
      }
    }
  );

  const jobs = () => (tasksData()?.jobs || []).filter((j) => RELEVANT_KINDS.includes(j.kind));
  const hasJobs = () => jobs().length > 0;

  // Report whether any relevant job is in a running-ish state so
  // the parent can disable the Process button while a fetch or
  // decomposition is in flight.
  const hasRunningJob = () => jobs().some((j) => RUNNING_STATES.includes(j.state));
  createEffect(() => {
    props.onRunningChange?.(hasRunningJob());
  });

  return (
    <div class="border-t dark:border-gray-700 pt-2 space-y-2">
      <div class="flex items-center gap-2 flex-wrap">
        <span class="text-xs font-medium text-gray-500 dark:text-gray-400">Tasks:</span>
        <Show
          when={hasJobs()}
          fallback={
            <span class="text-xs text-gray-400 dark:text-gray-500">no jobs yet</span>
          }
        >
          <For each={jobs()}>
            {(job) => (
              <button
                type="button"
                class="inline-flex items-center gap-1 text-xs hover:underline"
                onClick={() =>
                  setExpandedJob((cur) => (cur && cur.id === job.id ? null : job))
                }
                title={`View ${job.kind} job #${job.id}`}
              >
                <Badge variant={STATE_BADGE[job.state] || "gray"}>{job.kind}</Badge>
                <span class="text-gray-500 dark:text-gray-400">#{job.id} {job.state}</span>
              </button>
            )}
          </For>
          <Button variant="secondary" class="text-xs px-2 py-0.5" onClick={refetch}>
            Refresh
          </Button>
        </Show>
      </div>
      <Show when={expandedJob()}>
        <div class="border dark:border-gray-700 rounded p-3 bg-gray-50 dark:bg-gray-900/40">
          <JobDetail job={expandedJob()} onBack={() => setExpandedJob(null)} />
        </div>
      </Show>
    </div>
  );
}