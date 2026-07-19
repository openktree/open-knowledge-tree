import { Show } from "solid-js";
import Badge from "./Badge";
import Button from "./Button";

const existingVariant = (status) => {
  switch (status) {
    case "fetched":
    case "processed":
      return "green";
    case "pending":
    case "fetching":
      return "yellow";
    case "failed":
      return "red";
    default:
      return "gray";
  }
};

// SearchResult is the shared row rendered by both the Providers
// page's TestSearchPanel and the Investigation page's
// SearchAndRetrievePanel. It renders the result title/url/snippet
// and the "Already added" badge the same way everywhere; the
// action area is data-driven by optional props so each page wires
// only the behavior it needs:
//
//   - onClassify + classify(): Providers' "Classify" button and
//     type/strategy badges. Omit on Investigation.
//   - onFetch(result, process): the fetch handler. Both pages pass
//     this. `result` is the full search result object (so the
//     Investigation flow can read result.doi); the Providers page
//     re-derives url/doi inside its own handler.
//   - fetchLabel / fetchProcessLabel: button text. Defaults to
//     "Fetch" / "Fetch & Process"; Investigation passes
//     "Fetch & Add" / "Fetch & Process".
//   - progress(): optional per-result progress object
//     { stage: "fetching"|"polling"|"done"|"error", jobId?, error? }.
//     When present, the row renders stage badges and disables
//     buttons while in flight. Providers omits this and uses
//     enqueue() instead.
//   - enqueue(): optional one-shot enqueue result
//     { loading, jobId, classifiedAs, error } — Providers'
//     legacy "queued" badge. Ignored when progress() is present.
export default function SearchResult(props) {
  const r = () => props.result;
  const progress = () => props.progress;
  const stage = () => progress()?.stage;
  const enqueue = () => props.enqueue;
  const classify = () => props.classify;

  const inFlight = () => stage() === "fetching" || stage() === "polling" || enqueue()?.loading;
  const fetchDisabled = () => inFlight() || r().already_exists;

  const stageBadge = () => {
    switch (stage()) {
      case "fetching":
        return <Badge variant="blue">fetching...</Badge>;
      case "polling":
        return <Badge variant="blue">processing job #{progress()?.jobId}</Badge>;
      case "done":
        return <Badge variant="green">{props.doneLabel || "added to investigation"}</Badge>;
      case "error":
        return <Badge variant="red">failed</Badge>;
      default:
        return null;
    }
  };

  const fetchLabel = () => props.fetchLabel || "Fetch";
  const fetchProcessLabel = () => props.fetchProcessLabel || "Fetch & Process";
  const reFetchLabel = () => props.reFetchLabel || "Re-fetch";
  const reFetchProcessLabel = () => props.reFetchProcessLabel || "Re-fetch & Process";

  const hasFetchStage = () => !!stage();
  const doneOrReFetch = () => (stage() === "done" ? reFetchLabel() : fetchLabel());
  const doneOrReFetchProcess = () =>
    stage() === "done" ? reFetchProcessLabel() : fetchProcessLabel();

  return (
    <div class="border border-border rounded p-3">
      <div class="flex items-start gap-2">
        <a
          href={r().url}
          target="_blank"
          rel="noopener noreferrer"
          class="text-link hover:underline text-sm font-medium block flex-1 min-w-0"
        >
          {r().title}
        </a>
        <Show when={r().already_exists}>
          <Badge variant={existingVariant(r().existing_status)}>
            Already added{r().existing_status ? `: ${r().existing_status}` : ""}
          </Badge>
        </Show>
      </div>
      <p class="text-xs text-text-muted mt-0.5 truncate">{r().url}</p>
      <Show when={r().snippet}>
        <p class="text-xs text-text-muted mt-1">{r().snippet}</p>
      </Show>
      <div class="mt-2 flex items-center gap-2 flex-wrap">
        <Show when={props.onClassify}>
          <Button
            variant="secondary"
            onClick={() => props.onClassify(r().url)}
            loading={classify()?.loading}
            loadingText="Classifying..."
            class="text-xs px-2 py-1"
          >
            {classify() && !classify().loading ? "Re-classify" : "Classify"}
          </Button>
        </Show>
        <Show
          when={!r().already_exists}
          fallback={<span class="text-xs text-text-muted">fetch disabled</span>}
        >
          <Button
            variant="primary"
            onClick={() => props.onFetch(r(), false)}
            disabled={fetchDisabled()}
            loading={stage() === "fetching" || enqueue()?.loading}
            loadingText="Enqueuing..."
            class="text-xs px-2 py-1"
          >
            {doneOrReFetch()}
          </Button>
          <Button
            variant="primary"
            onClick={() => props.onFetch(r(), true)}
            disabled={fetchDisabled()}
            loading={stage() === "fetching" || enqueue()?.loading}
            loadingText="Enqueuing..."
            class="text-xs px-2 py-1"
          >
            {doneOrReFetchProcess()}
          </Button>
        </Show>

        {stageBadge()}
        <Show when={stage() === "error"}>
          <span class="text-xs text-danger">{progress()?.error}</span>
        </Show>

        <Show when={classify() && !classify().loading && !classify()?.error && props.onClassify}>
          <Badge variant="green">type: {classify().type}</Badge>
          <Badge variant="purple">
            strategy: {(classify().strategy || []).join(" \u2192 ") || "none"}
          </Badge>
        </Show>
        <Show when={classify()?.error}>
          <span class="text-xs text-danger">{classify().error}</span>
        </Show>

        <Show when={!hasFetchStage() && enqueue() && !enqueue().loading}>
          <Show when={enqueue().jobId}>
            <Badge variant="blue">queued: {enqueue().classifiedAs}</Badge>
            <span class="text-xs text-text-muted" title={enqueue().value}>
              job {enqueue().jobId}
            </span>
          </Show>
          <Show when={enqueue()?.error}>
            <span class="text-xs text-danger">fetch: {enqueue().error}</span>
          </Show>
        </Show>
      </div>
    </div>
  );
}
