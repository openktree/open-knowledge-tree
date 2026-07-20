export function formatTimestamp(ts) {
  if (!ts) return "";
  const ms =
    typeof ts === "string"
      ? new Date(ts).getTime()
      : ts instanceof Date
        ? ts.getTime()
        : Number.isNaN(ts)
          ? NaN
          : ts;
  if (Number.isNaN(ms)) return "";
  return new Date(ms).toLocaleString();
}
