// Breakdown tabs for the dashboard. Each tab switches the
// breakdown table's grouping dimension. The summary + over-time
// chart are always visible above the tabbed breakdown.
export const BREAKDOWN_TABS = [
  { id: "model", label: "Per Model" },
  { id: "operation", label: "Per Operation" },
  { id: "repository", label: "Per Repository" },
  { id: "source", label: "Per Source" },
];

// Time-bucket options for the over-time chart.
export const BUCKET_OPTIONS = [
  { value: "day", label: "Daily" },
  { value: "week", label: "Weekly" },
  { value: "month", label: "Monthly" },
];

// Operation label + badge color map. Used by the breakdown table
// to render the operation column consistently.
export const OPERATION_BADGE = {
  chat: "blue",
  embedding: "purple",
  fact_extraction: "green",
};

export const OPERATION_LABEL = {
  chat: "Chat",
  embedding: "Embedding",
  fact_extraction: "Fact Extraction",
};

// formatNumber renders an integer with thousands separators so
// the stat cards don't run together for large token counts.
export function formatNumber(n) {
  if (n == null) return "0";
  return Number(n).toLocaleString();
}

// formatCost renders a dollar amount with 4 decimal places when
// small (sub-cent costs are common for cheap models) and 2
// decimals once over $1.
export function formatCost(c) {
  if (c == null) return "$0.00";
  if (c < 1) return `$${c.toFixed(4)}`;
  return `$${c.toFixed(2)}`;
}

// formatBucket renders a time bucket timestamp according to the
// bucket width: daily → short date, weekly → week-of label,
// monthly → month label.
export function formatBucket(ts, bucket) {
  if (!ts) return "\u2014";
  const d = new Date(ts);
  switch (bucket) {
    case "month":
      return d.toLocaleDateString(undefined, { year: "numeric", month: "short" });
    case "week":
      return `wk of ${d.toLocaleDateString(undefined, { month: "short", day: "numeric" })}`;
    default:
      return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
  }
}
