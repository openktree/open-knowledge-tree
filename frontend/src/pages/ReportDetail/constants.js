export const statusVariant = {
  pending: "bg-yellow-100 text-yellow-800 dark:bg-yellow-900 dark:text-yellow-200",
  processing: "bg-blue-100 text-blue-800 dark:bg-blue-900 dark:text-blue-200",
  annotated: "bg-green-100 text-green-800 dark:bg-green-900 dark:text-green-200",
  failed: "bg-red-100 text-red-800 dark:bg-red-900 dark:text-red-200",
};

export function formatTimestamp(ts) {
  if (!ts) return "";
  try {
    return new Date(ts).toLocaleString();
  } catch {
    return ts;
  }
}

// formatScore renders a cosine similarity (0..1) as a percentage with
// one decimal place so the UI can surface how strong each match is.
export function formatScore(score) {
  if (score == null) return "—";
  return `${(score * 100).toFixed(1)}%`;
}

// postureStyle maps an autocite posture label (returned by the LLM
// classifier) to a colored badge class + short label. Empty/null
// posture (legacy/fallback rows, or when the classifier was not
// configured) renders no badge — the caller wraps this in a Show.
export const postureStyle = {
  supports: {
    label: "supports",
    class: "bg-green-100 text-green-800 dark:bg-green-900 dark:text-green-200",
  },
  contradicts: {
    label: "contradicts",
    class: "bg-red-100 text-red-800 dark:bg-red-900 dark:text-red-200",
  },
  related: {
    label: "related",
    class: "bg-blue-100 text-blue-800 dark:bg-blue-900 dark:text-blue-200",
  },
};
