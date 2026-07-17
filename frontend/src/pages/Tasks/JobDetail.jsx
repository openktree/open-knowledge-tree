import { createResource, Show, For } from "solid-js";
import { api } from "../../services/api";
import Card from "../../components/Card";
import Badge from "../../components/Badge";
import Button from "../../components/Button";
import { STATE_BADGE, formatDurationMs } from "./constants";
import { useNowTicker, resolveJobDuration } from "./useNowTicker";
import TraceTable from "./TraceTable";

export default function JobDetail(props) {
  const [jobData, { refetch }] = createResource(
    () => props.job.id,
    async (id) => {
      try {
        return await api.getTask(id);
      } catch {
        return null;
      }
    }
  );

  const job = () => jobData() || props.job;
  const now = useNowTicker();

  return (
    <div class="space-y-6">
      <div class="flex items-center gap-4">
        <Button variant="secondary" onClick={props.onBack}>Back to list</Button>
        <Button variant="secondary" onClick={refetch}>Refresh</Button>
      </div>

      <Card>
        <h2 class="text-lg font-semibold mb-4 dark:text-white">
          Job #{job().id}
        </h2>
        <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
          <div>
            <dt class="text-sm font-medium text-gray-500 dark:text-gray-400">Kind</dt>
            <dd class="mt-1 text-sm font-mono dark:text-gray-200">{job().kind}</dd>
          </div>
          <div>
            <dt class="text-sm font-medium text-gray-500 dark:text-gray-400">State</dt>
            <dd class="mt-1">
              <Badge variant={STATE_BADGE[job().state] || "gray"}>{job().state}</Badge>
            </dd>
          </div>
          <div>
            <dt class="text-sm font-medium text-gray-500 dark:text-gray-400">Queue</dt>
            <dd class="mt-1 text-sm font-mono dark:text-gray-200">{job().queue}</dd>
          </div>
          <div>
            <dt class="text-sm font-medium text-gray-500 dark:text-gray-400">Attempt</dt>
            <dd class="mt-1 text-sm dark:text-gray-200">{job().attempt}/{job().max_attempts}</dd>
          </div>
          <div>
            <dt class="text-sm font-medium text-gray-500 dark:text-gray-400">Priority</dt>
            <dd class="mt-1 text-sm dark:text-gray-200">{job().priority}</dd>
          </div>
          <div>
            <dt class="text-sm font-medium text-gray-500 dark:text-gray-400">Created</dt>
            <dd class="mt-1 text-sm dark:text-gray-200">
              {job().created_at ? new Date(job().created_at).toLocaleString() : "\u2014"}
            </dd>
          </div>
          <div>
            <dt class="text-sm font-medium text-gray-500 dark:text-gray-400">Scheduled</dt>
            <dd class="mt-1 text-sm dark:text-gray-200">
              {job().scheduled_at ? new Date(job().scheduled_at).toLocaleString() : "\u2014"}
            </dd>
          </div>
          <div>
            <dt class="text-sm font-medium text-gray-500 dark:text-gray-400">Attempted</dt>
            <dd class="mt-1 text-sm dark:text-gray-200">
              {job().attempted_at ? new Date(job().attempted_at).toLocaleString() : "\u2014"}
            </dd>
          </div>
          <div>
            <dt class="text-sm font-medium text-gray-500 dark:text-gray-400">Finalized</dt>
            <dd class="mt-1 text-sm dark:text-gray-200">
              {job().finalized_at ? new Date(job().finalized_at).toLocaleString() : "\u2014"}
            </dd>
          </div>
          <div>
            <dt class="text-sm font-medium text-gray-500 dark:text-gray-400">Duration</dt>
            <dd class="mt-1 text-sm font-mono dark:text-gray-200">
              {formatDurationMs(resolveJobDuration(job(), now()))}
            </dd>
          </div>
          <div>
            <dt class="text-sm font-medium text-gray-500 dark:text-gray-400">Queue Wait</dt>
            <dd class="mt-1 text-sm font-mono dark:text-gray-200">
              {job().queue_wait_ms != null ? formatDurationMs(job().queue_wait_ms) : "\u2014"}
            </dd>
          </div>
        </div>
      </Card>

      <Show when={job().encoded_args}>
        <Card>
          <h3 class="text-md font-semibold mb-2 dark:text-white">Arguments</h3>
          <pre class="text-xs bg-gray-50 dark:bg-gray-900 p-3 rounded overflow-x-auto font-mono text-gray-700 dark:text-gray-300">
            {JSON.stringify(job().encoded_args, null, 2)}
          </pre>
        </Card>
      </Show>

      <Show when={job().output}>
        <Card>
          <h3 class="text-md font-semibold mb-2 dark:text-white">Output</h3>
          <Show when={job().kind === "source_decomposition" && job().output}>
            <ResultSummary output={job().output} />
            <TraceTable output={job().output} />
          </Show>
          <Show when={job().kind === "embed_facts" && job().output}>
            <EmbedSummary output={job().output} />
          </Show>
          <pre class="text-xs bg-gray-50 dark:bg-gray-900 p-3 rounded overflow-x-auto font-mono text-gray-700 dark:text-gray-300">
            {JSON.stringify(job().output, null, 2)}
          </pre>
        </Card>
      </Show>

      <Show when={job().metadata}>
        <Card>
          <h3 class="text-md font-semibold mb-2 dark:text-white">Metadata</h3>
          <pre class="text-xs bg-gray-50 dark:bg-gray-900 p-3 rounded overflow-x-auto font-mono text-gray-700 dark:text-gray-300">
            {JSON.stringify(job().metadata, null, 2)}
          </pre>
        </Card>
      </Show>

      <Show when={job().errors && job().errors.length > 0}>
        <Card>
          <h3 class="text-md font-semibold mb-2 dark:text-white">Errors</h3>
          <div class="space-y-3">
            <For each={job().errors}>
              {(err) => (
                <div class="border border-red-200 dark:border-red-800 rounded p-3 bg-red-50 dark:bg-red-900/20">
                  <div class="flex items-center gap-2 mb-1">
                    <Badge variant="red">Attempt {err.attempt}</Badge>
                    <span class="text-xs text-gray-500 dark:text-gray-400">
                      {err.at ? new Date(err.at).toLocaleString() : ""}
                    </span>
                  </div>
                  <p class="text-sm text-red-700 dark:text-red-300 font-mono">{err.error}</p>
                  <Show when={err.trace}>
                    <pre class="mt-2 text-xs text-gray-600 dark:text-gray-400 overflow-x-auto font-mono whitespace-pre-wrap">
                      {err.trace}
                    </pre>
                  </Show>
                </div>
              )}
            </For>
          </div>
        </Card>
      </Show>

      <Show when={job().tags && job().tags.length > 0}>
        <Card>
          <h3 class="text-md font-semibold mb-2 dark:text-white">Tags</h3>
          <div class="flex flex-wrap gap-2">
            <For each={job().tags}>
              {(tag) => <Badge variant="gray">{tag}</Badge>}
            </For>
          </div>
        </Card>
      </Show>
    </div>
  );
}

// EmbedSummary renders a human-readable breakdown of an embed_facts
// job's output: how many facts were vectorized, the model used, and
// whether any facts failed the mark-embedded step. Kept private to
// this folder because it's specific to the embed_facts output shape.
function EmbedSummary(props) {
  const o = () => props.output || {};
  return (
    <div class="mb-3">
      <div class="flex flex-wrap gap-2">
        <Badge variant="blue">Embedded: {o().embedded ?? 0}</Badge>
        <Badge variant="gray">Model: {o().model ?? "\u2014"}</Badge>
        <Show when={o().embed_errors > 0}>
          <Badge variant="red">Errors: {o().embed_errors}</Badge>
        </Show>
      </div>
    </div>
  );
}

// ResultSummary renders a human-readable breakdown of a
// source_decomposition job's output: how many chunks/images were
// processed, how many facts were produced, and — critically — how
// many chunks/images failed extraction. Failures are surfaced as a
// red warning banner so a job that "completed" but actually timed
// out on every chunk is visible at a glance, not buried in the raw
// JSON. Kept private to this folder because it's specific to the
// source_decomposition output shape.
function ResultSummary(props) {
  const o = () => props.output || {};
  const hasFailures = () =>
    (o().chunk_failures && o().chunk_failures > 0) ||
    (o().image_failures && o().image_failures > 0);
  const allChunksFailed = () =>
    o().chunks > 0 && o().chunk_failures === o().chunks && o().facts === 0;

  return (
    <div class="mb-3 space-y-2">
      <div class="flex flex-wrap gap-2">
        <Badge variant="gray">Chunks: {o().chunks ?? 0}</Badge>
        <Badge variant="blue">Facts: {o().facts ?? 0}</Badge>
        <Badge variant="gray">Images: {o().images ?? 0}</Badge>
        <Show when={o().chunk_failures > 0}>
          <Badge variant="red">Chunk failures: {o().chunk_failures}</Badge>
        </Show>
        <Show when={o().image_failures > 0}>
          <Badge variant="red">Image failures: {o().image_failures}</Badge>
        </Show>
      </div>
      <Show when={hasFailures()}>
        <div class={`rounded p-3 text-sm ${allChunksFailed() ? "bg-red-50 dark:bg-red-900/20 border border-red-200 dark:border-red-800" : "bg-yellow-50 dark:bg-yellow-900/20 border border-yellow-200 dark:border-yellow-800"}`}>
          <p class={allChunksFailed() ? "text-red-700 dark:text-red-300 font-medium" : "text-yellow-700 dark:text-yellow-300 font-medium"}>
            {allChunksFailed()
              ? `Extraction failed: all ${o().chunks} chunks errored and produced 0 facts. The job is marked errored — check the Errors section and the worker logs (likely a provider timeout or rate limit).`
              : `${o().chunk_failures ?? 0} chunk(s) and ${o().image_failures ?? 0} image(s) failed extraction, but ${o().facts ?? 0} fact(s) were still produced. The job completed with partial success.`}
          </p>
        </div>
      </Show>
    </div>
  );
}
