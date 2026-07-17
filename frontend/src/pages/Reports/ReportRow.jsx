import { createSignal, Show } from "solid-js";
import { A } from "@solidjs/router";
import { api } from "../../services/api";
import Button from "../../components/Button";
import { statusVariant, formatTimestamp, statusLabel } from "./constants";

export default function ReportRow(props) {
  const [confirmDelete, setConfirmDelete] = createSignal(false);
  const [busy, setBusy] = createSignal(false);
  const [copied, setCopied] = createSignal(false);

  const handleCopy = async () => {
    const text = props.report.body_md ?? "";
    try {
      if (navigator.clipboard?.writeText) {
        await navigator.clipboard.writeText(text);
      } else {
        const ta = document.createElement("textarea");
        ta.value = text;
        ta.style.position = "fixed";
        ta.style.opacity = "0";
        document.body.appendChild(ta);
        ta.select();
        document.execCommand("copy");
        document.body.removeChild(ta);
      }
      setCopied(true);
      props.onAlert?.({ variant: "success", message: "Report copied to clipboard" });
      setTimeout(() => setCopied(false), 1500);
    } catch (err) {
      props.onAlert?.({ variant: "error", message: err.message || "Failed to copy report" });
    }
  };

  const handleDelete = async () => {
    setBusy(true);
    try {
      await api.deleteReport(props.slug, props.report.id);
      props.onDeleted?.(props.report.id);
    } catch (err) {
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setBusy(false);
      setConfirmDelete(false);
    }
  };

  return (
    <tr class="border-b border-gray-100 dark:border-gray-700">
      <td class="py-2 pr-4">
        <div class="flex items-center" style={{ "padding-left": `${(props.depth || 0) * 1.25}rem` }}>
          <Show when={props.hasChildren}>
            <button
              type="button"
              onClick={() => props.onToggle?.()}
              class="mr-1.5 inline-flex items-center justify-center w-5 h-5 rounded hover:bg-gray-100 dark:hover:bg-gray-700 text-gray-500 dark:text-gray-400 text-xs select-none flex-shrink-0"
              aria-label={props.expanded ? "Collapse" : "Expand"}
            >
              {props.expanded ? "▾" : "▸"}
            </button>
          </Show>
          <Show when={!props.hasChildren}>
            <span class="inline-block w-5 mr-1.5 flex-shrink-0" />
          </Show>
          <div>
            <A href={`/${props.slug}/reports/${props.report.id}`} class="text-blue-600 dark:text-blue-400 hover:underline font-medium">
              {props.report.title}
            </A>
            <Show when={props.report.topic}>
              <p class="text-xs text-gray-500 dark:text-gray-400">{props.report.topic}</p>
            </Show>
          </div>
        </div>
      </td>
      <td class="py-2 pr-4">
        <span class={`inline-block px-2 py-0.5 rounded text-xs font-medium ${statusVariant[statusLabel(props.report.status)] || ""}`}>
          {statusLabel(props.report.status)}
        </span>
      </td>
      <td class="py-2 pr-4 text-gray-600 dark:text-gray-300">
        {props.report.sentence_count ?? "—"}
      </td>
      <td class="py-2 pr-4 text-xs text-gray-500 dark:text-gray-400">
        {formatTimestamp(props.report.created_at)}
      </td>
      <td class="py-2 pr-4">
        <Show when={!confirmDelete()} fallback={
          <span class="inline-flex gap-1">
            <Button variant="danger" loading={busy()} onClick={handleDelete}>Confirm</Button>
            <Button variant="secondary" onClick={() => setConfirmDelete(false)}>Cancel</Button>
          </span>
        }>
          <span class="inline-flex gap-1">
            <Button variant="secondary" onClick={handleCopy}>{copied() ? "Copied" : "Copy"}</Button>
            <Button variant="danger" onClick={() => setConfirmDelete(true)}>Delete</Button>
          </span>
        </Show>
      </td>
    </tr>
  );
}