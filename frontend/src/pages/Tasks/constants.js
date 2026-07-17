export const STATE_BADGE = {
  available: "green",
  running: "blue",
  completed: "green",
  retryable: "yellow",
  cancelled: "red",
  discarded: "red",
  scheduled: "purple",
  pending: "gray",
};

export const STATE_OPTIONS = [
  { value: "", label: "All states" },
  { value: "available", label: "Available" },
  { value: "running", label: "Running" },
  { value: "completed", label: "Completed" },
  { value: "retryable", label: "Retryable" },
  { value: "cancelled", label: "Cancelled" },
  { value: "discarded", label: "Discarded" },
  { value: "scheduled", label: "Scheduled" },
  { value: "pending", label: "Pending" },
];

// KIND_OPTIONS and QUEUE_OPTIONS list every River worker kind
// registered in backend/internal/taskmanager/taskmanager.go. Keep
// them in sync when a new worker is added — a missing entry here
// means the /tasks page can't filter to that kind (the jobs still
// show up under "All kinds", but the friendly label is gone and the
// dropdown filter can't select them).
export const KIND_OPTIONS = [
  { value: "", label: "All kinds" },
  { value: "retrieve_source", label: "Retrieve Source" },
  { value: "source_decomposition", label: "Source Decomposition" },
  { value: "embed_facts", label: "Embed Facts" },
  { value: "deduplicate_facts", label: "Deduplicate Facts" },
  { value: "extract_concepts", label: "Extract Concepts" },
  { value: "refine_concepts", label: "Refine Concepts (Alias Generation)" },
  { value: "embed_concepts", label: "Embed Concepts" },
  { value: "summarize_concepts", label: "Summarize Concepts" },
  { value: "synthesize_concept", label: "Synthesize Concept" },
  { value: "cleanup_facts", label: "Cleanup Facts" },
  { value: "fact_catchup", label: "Fact Catchup" },
  { value: "migrate_context", label: "Migrate Context" },
  { value: "contribute_source", label: "Contribute Source" },
  { value: "contribute_all", label: "Contribute All" },
  { value: "pull_all_from_registry", label: "Pull All From Registry" },
  { value: "pull_remote_batch", label: "Pull Remote Batch" },
  { value: "refresh_concept_relations", label: "Refresh Concept Relations" },
  { value: "refresh_all_concept_relations", label: "Refresh All Concept Relations" },
  { value: "annotate_report", label: "Annotate Report" },
];

export const QUEUE_OPTIONS = [
  { value: "", label: "All queues" },
  { value: "retrieve_source", label: "Retrieve Source" },
  { value: "source_decomposition", label: "Source Decomposition" },
  { value: "embed_facts", label: "Embed Facts" },
  { value: "deduplicate_facts", label: "Deduplicate Facts" },
  { value: "extract_concepts", label: "Extract Concepts" },
  { value: "refine_concepts", label: "Refine Concepts (Alias Generation)" },
  { value: "embed_concepts", label: "Embed Concepts" },
  { value: "summarize_concepts", label: "Summarize Concepts" },
  { value: "synthesize_concept", label: "Synthesize Concept" },
  { value: "cleanup_facts", label: "Cleanup Facts" },
  { value: "fact_catchup", label: "Fact Catchup" },
  { value: "migrate_context", label: "Migrate Context" },
  { value: "contribute_source", label: "Contribute Source" },
  { value: "contribute_all", label: "Contribute All" },
  { value: "pull_all_from_registry", label: "Pull All From Registry" },
  { value: "pull_remote_batch", label: "Pull Remote Batch" },
  { value: "refresh_concept_relations", label: "Refresh Concept Relations" },
  { value: "refresh_all_concept_relations", label: "Refresh All Concept Relations" },
  { value: "annotate_report", label: "Annotate Report" },
  { value: "default", label: "Default" },
];

// formatDurationMs renders a millisecond duration as a short
// human-readable string. The backend already sends duration_ms
// for terminal jobs (completed/cancelled/discarded) and a
// live counter for running/retryable. The function handles
// every shape the API can produce:
//
//   - null / undefined → "—" (the job has never been attempted;
//     there is nothing to measure).
//   - integer < 1s → "<1s" (sub-second runs are common for the
//     happy path; the precision gets noisy below 1s).
//   - seconds, minutes, hours, days are picked to match the
//     largest unit that still produces a non-zero component.
//   - the function does NOT prefix a sign for negative values
//     (a negative duration indicates clock skew between the
//     server and the worker; we just clamp to 0 upstream and
//     render the resulting 0 here as "—").
export function formatDurationMs(ms) {
  if (ms == null) return "\u2014";
  const total = Math.max(0, Math.floor(ms / 1000));
  if (total < 1) return "<1s";
  const days = Math.floor(total / 86400);
  const hours = Math.floor((total % 86400) / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const seconds = total % 60;
  if (days > 0) return hours > 0 ? `${days}d ${hours}h` : `${days}d`;
  if (hours > 0) return minutes > 0 ? `${hours}h ${minutes}m` : `${hours}h`;
  if (minutes > 0) return seconds > 0 ? `${minutes}m ${seconds}s` : `${minutes}m`;
  return `${seconds}s`;
}
