import { createResource, Show } from "solid-js";
import Button from "../../components/Button";
import { api } from "../../services/api";

// ReprocessDangerBox is the red-bordered confirmation panel that
// appears when the user clicks "Reprocess" on a source with chunk
// failures. It fetches a live preview (GET /sources/{id}/reprocess)
// of the failed chunk count and shows it in a danger-styled box
// before the user confirms.
//
// Props:
//   repoID        — the repository UUID
//   source        — the source object (must have id + chunk_failures)
//   reprocessing  — boolean signal from the parent (POST in flight)
//   onConfirm     — async fn(repoID, sourceID) → result (called on Confirm)
//   onCancel      — fn() (called on Cancel)
export default function ReprocessDangerBox(props) {
  const [preview] = createResource(
    () => props.repoID && props.source?.id,
    async ([repoID, sourceID]) => {
      if (!repoID || !sourceID) return null;
      try {
        return await api.previewReprocessSource(repoID, sourceID);
      } catch {
        return null;
      }
    },
  );

  return (
    <div class="border-2 border-red-400 dark:border-red-600 rounded-lg p-3 bg-red-50 dark:bg-red-950/30 mt-2">
      <Show when={preview.loading}>
        <p class="text-xs text-gray-400 mb-2">Loading chunk failure details…</p>
      </Show>

      <Show when={preview()}>
        <div class="text-xs text-red-800 dark:text-red-300 space-y-1 mb-3">
          <p class="font-semibold">Re-run failed chunks for "{preview().source_title}"</p>
          <ul class="list-disc list-inside space-y-0.5 pl-1">
            <li>
              <span class="font-mono font-bold">{preview().failed_chunk_count}</span> failed
              chunk(s) will be re-extracted
            </li>
            <li>Successful chunks will NOT be re-run (no duplicate facts)</li>
          </ul>
          <p class="text-red-600 dark:text-red-400 italic">
            This spends LLM quota on the failed chunks only. Confirm to proceed.
          </p>
        </div>
      </Show>

      <div class="flex items-center gap-2">
        <Button
          variant="danger"
          onClick={() => props.onConfirm(props.repoID, props.source.id)}
          loading={props.reprocessing}
          loadingText="Enqueuing..."
          disabled={preview.loading}
        >
          Confirm reprocess
        </Button>
        <Button variant="secondary" onClick={props.onCancel} disabled={props.reprocessing}>
          Cancel
        </Button>
      </div>
    </div>
  );
}
