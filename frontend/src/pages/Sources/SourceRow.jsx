import { Show } from "solid-js";
import { A } from "@solidjs/router";
import Badge from "../../components/Badge";
import Button from "../../components/Button";
import { statusVariant, formatTimestamp, oaStatusVariant, oaStatusCopy } from "./constants";

export default function SourceRow(props) {
  const source = () => props.source;
  const detailHref = () => `/${props.slug}/sources/${source().id}`;
  return (
    <div class="border rounded dark:border-gray-700 hover:border-blue-400 dark:hover:border-blue-500 transition">
      <div class="flex items-center justify-between p-3 gap-3">
        <A
          href={detailHref()}
          class="min-w-0 flex-1 group"
        >
          <p
            class="text-blue-600 dark:text-blue-400 group-hover:underline text-sm font-medium block truncate"
            title={source().url}
          >
            {source().parsed_title && source().parsed_title.trim().length > 0
              ? source().parsed_title
              : source().url}
          </p>
          <Show when={source().parsed_title && source().parsed_title.trim().length > 0}>
            <p class="text-xs text-gray-500 dark:text-gray-400 truncate" title={source().url}>
              {source().url}
            </p>
          </Show>
          <div class="flex items-center gap-2 mt-1 flex-wrap text-xs text-gray-500 dark:text-gray-400">
            <Badge variant={statusVariant(source().status)}>{source().status}</Badge>
            <Show when={source().parse_status}>
              <Badge variant={
                source().parse_status === "ok" ? "green"
                : source().parse_status === "failed" ? "red"
                : "yellow"
              }>
                {source().parse_status}
              </Badge>
            </Show>
            <Show when={source().kind}>
              <Badge variant="gray">{source().kind}</Badge>
            </Show>
            <Show when={source().oa_status}>
              <Badge variant={oaStatusVariant[source().oa_status] || "gray"}>
                {oaStatusCopy[source().oa_status] || source().oa_status}
              </Badge>
            </Show>
            <Show when={source().fetched_at && source().fetched_at.Valid}>
              <span>fetched {formatTimestamp(source().fetched_at)}</span>
            </Show>
            <Show when={source().error}>
              <span class="text-red-600 dark:text-red-400" title={source().error}>
                {source().error.length > 80
                  ? source().error.slice(0, 80) + "..."
                  : source().error}
              </span>
            </Show>
          </div>
        </A>
        <Show when={props.canProcess}>
          <Button
            variant={props.processDisabled ? "secondary" : "primary"}
            onClick={props.onProcess}
            loading={props.processing}
            loadingText="..."
            disabled={props.processDisabled}
            class="text-xs px-2 py-1"
          >
            {props.processDisabled ? "Processed" : "Process"}
          </Button>
        </Show>
        <Show when={props.canRetry}>
          <Button
            variant="primary"
            onClick={props.onRetry}
            loading={props.retrying}
            loadingText="Retrying..."
            class="text-xs px-2 py-1"
          >
            Retry
          </Button>
        </Show>
        <Show when={props.canDelete}>
          <Button
            variant="danger"
            onClick={props.onDelete}
            loading={props.deleting}
            loadingText="Deleting..."
            class="text-xs px-2 py-1"
          >
            Delete
          </Button>
        </Show>
      </div>
    </div>
  );
}
