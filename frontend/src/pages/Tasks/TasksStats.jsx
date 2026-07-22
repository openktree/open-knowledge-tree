import { createSignal, For, Show } from "solid-js";
import Badge from "../../components/Badge";
import Button from "../../components/Button";
import Card from "../../components/Card";
import { STATE_BADGE } from "./constants";
import ReextractDangerBox from "./ReextractDangerBox";

const badgeVariant = (state) => STATE_BADGE[state] || "gray";

const SHORT_LABELS = {
  available: "avail",
  completed: "done",
  retryable: "retry",
  cancelled: "canc",
  discarded: "disc",
  scheduled: "sched",
  pending: "pend",
};

const shortLabel = (state) => SHORT_LABELS[state] || state;

function StatBadge(props) {
  return (
    <Badge variant={badgeVariant(props.state)} class="shrink-0">
      {props.count}
      <span class="hidden sm:inline"> {props.state}</span>
      <span class="sm:hidden"> {shortLabel(props.state)}</span>
    </Badge>
  );
}

export default function TasksStats(props) {
  const [expanded, setExpanded] = createSignal(false);
  const [confirmRescue, setConfirmRescue] = createSignal(false);
  const [rescueResult, setRescueResult] = createSignal(null);
  const [showReextract, setShowReextract] = createSignal(false);
  const total = () => props.stats?.totals?.total ?? 0;
  const hasData = () => props.stats?.queues?.length > 0;
  const runningCount = () => props.stats?.totals?.running ?? 0;

  async function handleRescue() {
    setConfirmRescue(false);
    const result = await props.onRescue();
    if (result) {
      setRescueResult(result);
      setTimeout(() => setRescueResult(null), 8000);
    }
  }

  return (
    <Card>
      <Show
        when={!props.loading}
        fallback={
          <div class="space-y-3">
            <div class="h-6 bg-gray-200 dark:bg-gray-700 rounded animate-pulse w-1/3" />
            <div class="flex gap-2">
              <div class="h-5 w-20 bg-gray-200 dark:bg-gray-700 rounded animate-pulse" />
              <div class="h-5 w-20 bg-gray-200 dark:bg-gray-700 rounded animate-pulse" />
              <div class="h-5 w-24 bg-gray-200 dark:bg-gray-700 rounded animate-pulse" />
            </div>
          </div>
        }
      >
        <Show when={hasData()}>
          <div class="flex items-baseline justify-between gap-4">
            <h2 class="text-lg font-semibold dark:text-white">Task Overview</h2>
            <div class="flex items-center gap-3 shrink-0">
              <Show when={props.onRefresh}>
                <Button
                  variant="secondary"
                  onClick={props.onRefresh}
                  loading={props.refreshing}
                  loadingText="..."
                >
                  Refresh
                </Button>
              </Show>
              <Show when={props.canRescue && runningCount() > 0}>
                <Show
                  when={!confirmRescue()}
                  fallback={
                    <div class="flex items-center gap-2">
                      <span class="text-xs text-gray-500 dark:text-gray-400">
                        Reset {runningCount()} running?
                      </span>
                      <Button
                        variant="danger"
                        onClick={handleRescue}
                        loading={props.rescuing}
                        loadingText="Resetting..."
                      >
                        Confirm
                      </Button>
                      <Button
                        variant="secondary"
                        onClick={() => setConfirmRescue(false)}
                        disabled={props.rescuing}
                      >
                        Cancel
                      </Button>
                    </div>
                  }
                >
                  <Button
                    variant="secondary"
                    onClick={() => setConfirmRescue(true)}
                    title="Reset running jobs owned by dead workers back to available"
                  >
                    Rescue stuck
                  </Button>
                </Show>
              </Show>
              <Show when={props.canReextract && (props.repositories?.length || 0) > 0}>
                <Button
                  variant="secondary"
                  onClick={() => setShowReextract(true)}
                  title="Clear retryable concept skips for a repo and re-enqueue concept extraction"
                >
                  Re-extract concepts
                </Button>
              </Show>
              <span class="text-xs text-gray-500 dark:text-gray-400 font-mono">
                {total()} total
              </span>
            </div>
          </div>

          <Show when={showReextract()}>
            <div class="mt-3">
              <ReextractDangerBox
                repositories={props.repositories}
                currentRepo={props.currentRepo}
                reextracting={props.reextracting}
                onConfirm={props.onReextract}
                onCancel={() => setShowReextract(false)}
              />
            </div>
          </Show>

          <Show when={rescueResult()}>
            <div class="mt-2 text-xs text-green-600 dark:text-green-400">
              Rescued {rescueResult().rescued} stuck job{rescueResult().rescued === 1 ? "" : "s"}{" "}
              (staleness threshold {rescueResult().threshold}).
            </div>
          </Show>

          <div class="flex flex-wrap gap-1.5 mt-3">
            <For each={Object.entries(props.stats.totals)}>
              {([state, count]) => (
                <Show when={count > 0 && state !== "total"}>
                  <StatBadge state={state} count={count} />
                </Show>
              )}
            </For>
          </div>

          <button
            onClick={() => setExpanded((v) => !v)}
            class="flex items-center gap-1.5 mt-3 text-xs text-gray-400 dark:text-gray-500 hover:text-gray-600 dark:hover:text-gray-300 transition-colors cursor-pointer"
          >
            <span class="text-[10px]">{expanded() ? "\u25BC" : "\u25B6"}</span>
            <span class="font-medium tracking-wide uppercase">
              {expanded() ? "Hide" : "Show"} details by queue
            </span>
          </button>

          <Show when={expanded()}>
            <div class="grid grid-cols-1 sm:grid-cols-2 gap-2 mt-2">
              <For each={props.stats.queues}>
                {(q) => (
                  <div class="border border-gray-200 dark:border-gray-700 rounded-lg p-3">
                    <div class="flex items-baseline justify-between mb-2 gap-2">
                      <span class="font-mono text-xs font-semibold text-gray-700 dark:text-gray-300 truncate">
                        {q.queue}
                      </span>
                      <div class="flex items-center gap-2 shrink-0">
                        <span class="text-[10px] text-gray-400 dark:text-gray-500 font-mono border border-gray-200 dark:border-gray-600 rounded px-1.5 py-0.5 leading-none">
                          cap {q.max_workers}
                        </span>
                        <span class="text-[11px] text-gray-400 dark:text-gray-500 font-mono">
                          {q.total}
                        </span>
                      </div>
                    </div>
                    <div class="flex flex-wrap gap-1">
                      <For each={Object.entries(q.states)}>
                        {([state, count]) => (
                          <Show when={count > 0}>
                            <StatBadge state={state} count={count} />
                          </Show>
                        )}
                      </For>
                    </div>
                  </div>
                )}
              </For>
            </div>
          </Show>
        </Show>

        <Show when={!hasData() && !props.loading}>
          <p class="text-sm text-gray-400 dark:text-gray-500">No task data available yet.</p>
        </Show>
      </Show>
    </Card>
  );
}
