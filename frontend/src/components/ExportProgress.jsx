import { createSignal, For, onCleanup, Show } from "solid-js";
import { api } from "../services/api";
import Badge from "./Badge";

const POLL_INTERVAL = 10000;

const EXPORT_PHASES = [
  { key: "building", label: "Building Bundle", icon: "B" },
  { key: "pushing", label: "Pushing to Registry", icon: "↑" },
  { key: "completed", label: "Completed", icon: "✓" },
];

const IMPORT_PHASES = [
  { key: "downloading", label: "Downloading", icon: "↓" },
  { key: "decoding", label: "Decoding Bundle", icon: "Z" },
  { key: "importing", label: "Importing", icon: "I" },
  { key: "completed", label: "Completed", icon: "✓" },
];

function formatBytes(bytes) {
  if (!bytes || bytes <= 0) return "";
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  return `${(bytes / (1024 * 1024 * 1024)).toFixed(2)} GB`;
}

function formatCount(n) {
  if (!n || n <= 0) return "";
  return n.toLocaleString();
}

export default function ExportProgress(props) {
  const jobKind = props.jobKind || "export_graph";
  const phases = jobKind === "import_graph" ? IMPORT_PHASES : EXPORT_PHASES;
  const [job, setJob] = createSignal(props.initialJob || null);
  const [error, setError] = createSignal(null);

  let pollTimer = null;
  let stopped = false;

  const poll = async () => {
    if (stopped) return;
    try {
      const data = await api.getTask(props.jobID);
      setJob(data);
      setError(null);
      const isFinal = ["completed", "cancelled", "discarded"].includes(data.state);
      if (isFinal) {
        stopped = true;
        if (pollTimer) clearInterval(pollTimer);
        props.onComplete?.(data);
      }
    } catch (err) {
      setError(err.message);
    }
  };

  if (!props.initialJob) {
    poll();
    pollTimer = setInterval(poll, POLL_INTERVAL);
  } else {
    const isFinal = ["completed", "cancelled", "discarded"].includes(props.initialJob.state);
    if (!isFinal) {
      pollTimer = setInterval(poll, POLL_INTERVAL);
    }
  }

  onCleanup(() => {
    stopped = true;
    if (pollTimer) clearInterval(pollTimer);
  });

  const progress = () => job()?.metadata?.progress || null;
  const currentPhase = () => progress()?.phase || "";
  const jobState = () => job()?.state || "running";
  const hasError = () =>
    progress()?.error || (jobState() === "discarded" && job()?.errors?.length > 0);
  const errorMsg = () =>
    progress()?.error || job()?.errors?.[job()?.errors.length - 1]?.error || "";

  const phaseIndex = (phase) => {
    const idx = phases.findIndex((p) => p.key === phase);
    return idx >= 0 ? idx : -1;
  };

  return (
    <div class="border border-gray-200 dark:border-gray-700 rounded-lg p-4 bg-gray-50 dark:bg-gray-900 space-y-3">
      <div class="flex items-center justify-between">
        <h4 class="text-sm font-semibold dark:text-white">
          {jobKind === "import_graph" ? "Import Progress" : "Export Progress"}
        </h4>
        <Badge
          variant={jobState() === "completed" ? "green" : jobState() === "running" ? "blue" : "red"}
        >
          {jobState()}
        </Badge>
      </div>

      <Show when={progress()}>
        <div class="space-y-1">
          <For each={phases}>
            {(phase) => {
              const idx = phaseIndex(phase.key);
              const currentIdx = phaseIndex(currentPhase());
              const isDone = currentIdx > idx;
              const isCurrent = currentPhase() === phase.key;
              return (
                <div class="flex items-center gap-2 text-sm">
                  <span
                    class={`inline-flex items-center justify-center w-5 h-5 rounded-full text-xs font-mono ${
                      isDone
                        ? "bg-green-500 text-white"
                        : isCurrent
                          ? "bg-blue-500 text-white animate-pulse"
                          : "bg-gray-200 dark:bg-gray-700 text-gray-400"
                    }`}
                  >
                    {isDone ? "✓" : phase.icon}
                  </span>
                  <span
                    class={
                      isDone || isCurrent
                        ? "text-gray-700 dark:text-gray-200"
                        : "text-gray-400 dark:text-gray-500"
                    }
                  >
                    {phase.label}
                  </span>
                </div>
              );
            }}
          </For>
        </div>
      </Show>

      <Show when={!progress() && jobState() === "running"}>
        <p class="text-sm text-gray-400 dark:text-gray-500">Waiting for progress updates…</p>
      </Show>

      <Show when={progress()?.source_count || progress()?.fact_count || progress()?.concept_count}>
        <div class="flex flex-wrap gap-4 text-xs text-gray-500 dark:text-gray-400">
          <Show when={progress()?.source_count}>
            <span>
              Sources:{" "}
              <strong class="text-gray-700 dark:text-gray-200">
                {formatCount(progress().source_count)}
              </strong>
            </span>
          </Show>
          <Show when={progress()?.fact_count}>
            <span>
              Facts:{" "}
              <strong class="text-gray-700 dark:text-gray-200">
                {formatCount(progress().fact_count)}
              </strong>
            </span>
          </Show>
          <Show when={progress()?.concept_count}>
            <span>
              Concepts:{" "}
              <strong class="text-gray-700 dark:text-gray-200">
                {formatCount(progress().concept_count)}
              </strong>
            </span>
          </Show>
          <Show when={progress()?.bundle_bytes}>
            <span>
              Bundle:{" "}
              <strong class="text-gray-700 dark:text-gray-200">
                {formatBytes(progress().bundle_bytes)}
              </strong>
            </span>
          </Show>
        </div>
      </Show>

      <Show when={progress()?.graph_id}>
        <div class="text-xs text-green-600 dark:text-green-400">
          Graph ID: <span class="font-mono">{progress().graph_id}</span>
        </div>
      </Show>

      <Show when={hasError()}>
        <div class="text-xs text-red-600 dark:text-red-400 bg-red-50 dark:bg-red-900/20 rounded p-2">
          <strong>Error:</strong> {errorMsg()}
        </div>
      </Show>

      <Show when={error()}>
        <div class="text-xs text-yellow-600 dark:text-yellow-400">Polling error: {error()}</div>
      </Show>

      <p class="text-xs text-gray-400 dark:text-gray-500">
        Auto-refreshes every {POLL_INTERVAL / 1000}s. <Show when={job()?.id}>Job #{job().id}</Show>
      </p>
    </div>
  );
}
