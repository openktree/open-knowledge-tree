import { For } from "solid-js";
import Card from "../../components/Card";
import Badge from "../../components/Badge";
import Button from "../../components/Button";
import { STATE_BADGE, formatDurationMs } from "./constants";
import { useNowTicker, resolveJobDuration } from "./useNowTicker";

// TasksTable renders the jobs list. The page owns the data; this
// component is controlled via props and calls back via onSelectJob.
// now ticks once a second so a running job's duration cell keeps
// moving between server refreshes.
export default function TasksTable(props) {
  const now = useNowTicker();
  return (
    <Card class="overflow-x-auto">
      <table class="w-full text-sm">
        <thead>
          <tr class="border-b border-gray-200 dark:border-gray-700 text-left">
            <th class="py-2 px-3 font-medium text-gray-500 dark:text-gray-400">ID</th>
            <th class="py-2 px-3 font-medium text-gray-500 dark:text-gray-400">Kind</th>
            <th class="py-2 px-3 font-medium text-gray-500 dark:text-gray-400">State</th>
            <th class="py-2 px-3 font-medium text-gray-500 dark:text-gray-400">Queue</th>
            <th class="py-2 px-3 font-medium text-gray-500 dark:text-gray-400">Attempt</th>
            <th class="py-2 px-3 font-medium text-gray-500 dark:text-gray-400">Priority</th>
            <th class="py-2 px-3 font-medium text-gray-500 dark:text-gray-400">Created</th>
            <th class="py-2 px-3 font-medium text-gray-500 dark:text-gray-400">Scheduled</th>
            <th class="py-2 px-3 font-medium text-gray-500 dark:text-gray-400">Duration</th>
            <th class="py-2 px-3 font-medium text-gray-500 dark:text-gray-400"></th>
          </tr>
        </thead>
        <tbody>
          <For each={props.jobs}>
            {(job) => (
              <tr class="border-b border-gray-100 dark:border-gray-700/50 hover:bg-gray-50 dark:hover:bg-gray-700/30">
                <td class="py-2 px-3 font-mono text-xs text-gray-600 dark:text-gray-400">{job.id}</td>
                <td class="py-2 px-3 font-mono text-xs dark:text-gray-300">{job.kind}</td>
                <td class="py-2 px-3">
                  <Badge variant={STATE_BADGE[job.state] || "gray"}>{job.state}</Badge>
                </td>
                <td class="py-2 px-3 font-mono text-xs text-gray-500 dark:text-gray-400">{job.queue}</td>
                <td class="py-2 px-3 text-xs text-gray-500 dark:text-gray-400">
                  {job.attempt}/{job.max_attempts}
                </td>
                <td class="py-2 px-3 text-xs text-gray-500 dark:text-gray-400">{job.priority}</td>
                <td class="py-2 px-3 text-xs text-gray-500 dark:text-gray-400">
                  {job.created_at ? new Date(job.created_at).toLocaleString() : "\u2014"}
                </td>
                <td class="py-2 px-3 text-xs text-gray-500 dark:text-gray-400">
                  {job.scheduled_at ? new Date(job.scheduled_at).toLocaleString() : "\u2014"}
                </td>
                <td class="py-2 px-3 text-xs text-gray-500 dark:text-gray-400 font-mono">
                  {formatDurationMs(resolveJobDuration(job, now()))}
                </td>
                <td class="py-2 px-3">
                  <Button variant="link" onClick={() => props.onSelectJob(job)}>View</Button>
                </td>
              </tr>
            )}
          </For>
        </tbody>
      </table>
    </Card>
  );
}