import { Show, createSignal } from "solid-js";
import { A } from "@solidjs/router";
import { api } from "../../services/api";
import Button from "../../components/Button";
import Badge from "../../components/Badge";
import SourceTaskStatus from "./SourceTaskStatus";
import { statusVariant, formatTimestamp } from "../Sources/constants";

export default function InvestigationSourceRow(props) {
  const src = () => props.source;
  const detailHref = () => `/${props.slug}/sources/${src().id}`;
  const [jobRunning, setJobRunning] = createSignal(false);

  const onRemove = async () => {
    try {
      await api.removeInvestigationSource(props.slug, props.invID, src().id);
      props.onRemoved?.();
    } catch (err) {
      props.onAlert?.({ variant: "error", message: err.message });
    }
  };

  // Mirrors the Sources page gate: the Process button shows when
  // the source is fetched with parseable text and not yet
  // processed. The backend's POST /sources/{id}/process enforces
  // the same precondition, so this is a UI affordance, not a guard.
  const canProcess = () =>
    src().status === "fetched" &&
    ((src().parsed_markdown && src().parsed_markdown.trim().length > 0) ||
      (src().parsed_text && src().parsed_text.trim().length > 0));
  const isProcessed = () => src().status === "processed";
  const processDisabled = () => props.processing || jobRunning();

  return (
    <div class="border rounded dark:border-gray-700 p-3 space-y-2">
      <div class="flex items-center justify-between gap-3">
        <A href={detailHref()} class="min-w-0 flex-1 group">
          <p
            class="text-blue-600 dark:text-blue-400 group-hover:underline text-sm font-medium truncate"
            title={src().url}
          >
            {src().parsed_title && src().parsed_title.trim().length > 0
              ? src().parsed_title
              : src().url}
          </p>
          <div class="flex items-center gap-2 mt-1 flex-wrap text-xs text-gray-500 dark:text-gray-400">
            <Badge variant={statusVariant(src().status)}>{src().status}</Badge>
            <Show when={src().parse_status}>
              <Badge variant={src().parse_status === "ok" ? "green" : src().parse_status === "failed" ? "red" : "yellow"}>
                {src().parse_status}
              </Badge>
            </Show>
            <Show when={src().kind}>
              <Badge variant="gray">{src().kind}</Badge>
            </Show>
            <Show when={src().fetched_at && src().fetched_at.Valid}>
              <span>fetched {formatTimestamp(src().fetched_at)}</span>
            </Show>
          </div>
        </A>
        <div class="flex items-center gap-1">
          <Show when={props.onProcess && canProcess() && !isProcessed()}>
            <Button
              variant="primary"
              class="text-xs px-2 py-1"
              disabled={processDisabled()}
              loading={props.processing}
              loadingText="..."
              onClick={() => props.onProcess(src())}
              title={jobRunning() ? "A fetch/decomposition job is still running for this source" : ""}
            >
              {jobRunning() ? "Running..." : "Process"}
            </Button>
          </Show>
          <Button variant="danger" class="text-xs px-2 py-1" onClick={onRemove}>
            Remove
          </Button>
        </div>
      </div>
      <SourceTaskStatus
        slug={props.slug}
        sourceID={src().id}
        onRunningChange={setJobRunning}
      />
    </div>
  );
}