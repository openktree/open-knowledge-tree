import { onCleanup, onMount, Show } from "solid-js";
import Card from "../../components/Card";
import { formatBucket } from "./constants";

// AIUsageChart renders a line chart of total tokens per time
// bucket, one series per model. The data comes from the by-day
// endpoint (`rows` = [{ bucket, model, total_tokens, ... }]).
// The chart is redrawn whenever the rows change; chart.js
// manages its own canvas sizing via the responsive/maintainAspect
// options.
//
// chart.js is imported dynamically inside onMount so the bundle
// stays lean on pages that never visit the dashboard, and so the
// canvas ref is guaranteed to exist before Chart is constructed.
export default function AIUsageChart(props) {
  let canvasRef;
  let chart;

  onMount(async () => {
    const {
      Chart,
      LineController,
      LineElement,
      PointElement,
      LinearScale,
      CategoryScale,
      Tooltip,
      Legend,
      Filler,
    } = await import("chart.js");
    Chart.register(
      LineController,
      LineElement,
      PointElement,
      LinearScale,
      CategoryScale,
      Tooltip,
      Legend,
      Filler,
    );
    chart = new Chart(canvasRef, {
      type: "line",
      data: buildChartData(props),
      options: {
        responsive: true,
        maintainAspectRatio: false,
        plugins: {
          legend: { labels: { color: getCSSColor("--tw-text-opacity, rgb(107 114 128)") } },
          tooltip: { mode: "index", intersect: false },
        },
        scales: {
          x: { ticks: { color: "rgb(107 114 128)" }, grid: { color: "rgba(107,114,128,0.15)" } },
          y: {
            ticks: { color: "rgb(107 114 128)" },
            grid: { color: "rgba(107,114,128,0.15)" },
            beginAtZero: true,
          },
        },
      },
    });
  });

  // Update the chart when the rows prop changes. We don't use a
  // createEffect because chart.js mutates the dataset arrays in
  // place; rebuilding data is simpler and avoids aliasing bugs.
  const update = () => {
    if (!chart) return;
    chart.data = buildChartData(props);
    chart.update();
  };
  // Solid doesn't have a built-in deep-watch; we call update from
  // the parent via a ref-less pattern: the parent re-mounts the
  // chart when the bucket changes (key=bucket), so a full
  // rebuild handles bucket switches. For row-only refreshes
  // (same bucket, new from/to), we rely on the createResource
  // refetch triggering a re-render; the chart reads the new
  // props on the next animation frame via this rAF loop.
  // Simpler: we poll update on prop change using on.
  let lastRows = props.rows;
  const tick = () => {
    if (props.rows !== lastRows) {
      lastRows = props.rows;
      update();
    }
    rafId = requestAnimationFrame(tick);
  };
  let rafId = requestAnimationFrame(tick);

  onCleanup(() => {
    cancelAnimationFrame(rafId);
    if (chart) chart.destroy();
  });

  return (
    <Card>
      <h2 class="text-lg font-semibold mb-3 dark:text-white">
        Consumption Over Time (
        {props.bucket === "month" ? "Monthly" : props.bucket === "week" ? "Weekly" : "Daily"})
      </h2>
      <Show
        when={(props.rows ?? []).length > 0}
        fallback={
          <p class="text-gray-400 dark:text-gray-500 text-sm py-8 text-center">
            No usage in range.
          </p>
        }
      >
        <div class="relative h-72">
          <canvas ref={canvasRef} />
        </div>
      </Show>
    </Card>
  );
}

// buildChartData pivots the flat by-day rows into the per-model
// line series chart.js expects. Buckets are sorted chronologically
// (the backend already orders by bucket); models are kept in
// first-seen order for stable colors across refreshes.
function buildChartData(props) {
  const rows = props.rows ?? [];
  const buckets = [];
  const seenBuckets = new Set();
  for (const r of rows) {
    const key = String(r.bucket);
    if (!seenBuckets.has(key)) {
      seenBuckets.add(key);
      buckets.push(r.bucket);
    }
  }
  const models = [];
  const seenModels = new Set();
  for (const r of rows) {
    if (!seenModels.has(r.model)) {
      seenModels.add(r.model);
      models.push(r.model);
    }
  }
  const datasets = models.map((model, i) => ({
    label: model,
    data: buckets.map((b) => {
      const row = rows.find((r) => String(r.bucket) === String(b) && r.model === model);
      return row ? row.total_tokens : 0;
    }),
    borderColor: MODEL_COLORS[i % MODEL_COLORS.length],
    backgroundColor: MODEL_COLORS[i % MODEL_COLORS.length] + "33",
    tension: 0.25,
    fill: false,
  }));
  return {
    labels: buckets.map((b) => formatBucket(b, props.bucket)),
    datasets,
  };
}

const MODEL_COLORS = [
  "#3b82f6", // blue
  "#a855f7", // purple
  "#22c55e", // green
  "#ef4444", // red
  "#eab308", // yellow
  "#06b6d4", // cyan
  "#ec4899", // pink
  "#f97316", // orange
];

function getCSSColor(_fallback) {
  // chart.js needs concrete colors; Tailwind vars don't expose
  // cleanly. Use a neutral gray that works in light + dark.
  return "rgb(107 114 128)";
}
