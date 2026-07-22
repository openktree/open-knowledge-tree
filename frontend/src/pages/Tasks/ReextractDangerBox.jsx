import { createResource, createSignal, For, Show } from "solid-js";
import Button from "../../components/Button";
import { api } from "../../services/api";

// ReextractDangerBox is the red-bordered confirmation panel that
// appears when the user clicks "Re-extract concepts" on the Tasks
// page. It fetches a live preview (GET /concepts/reextract) of the
// facts and candidates that would be affected, shows the counts in
// a danger-styled box, and lets the user confirm or cancel. On
// confirm it calls the POST endpoint via props.onConfirm.
//
// Props:
//   repositories     — array of {id, name} for the repo dropdown
//   currentRepo      — the currently selected repo object (optional preselect)
//   reextracting     — boolean signal from the parent (POST in flight)
//   onConfirm        — async fn(repoID) → result object (called on Confirm)
export default function ReextractDangerBox(props) {
  const [selectedRepo, setSelectedRepo] = createSignal(props.currentRepo?.id || "");
  const [result, setResult] = createSignal(null);

  const repoID = () => selectedRepo() || props.currentRepo?.id || "";

  const [preview] = createResource(repoID, async (id) => {
    if (!id) return null;
    try {
      return await api.previewReextractRepoConcepts(id);
    } catch {
      return null;
    }
  });

  async function handleConfirm() {
    const res = await props.onConfirm(repoID());
    if (res) {
      setResult(res);
      setTimeout(() => setResult(null), 10000);
    }
  }

  return (
    <div class="border-2 border-red-400 dark:border-red-600 rounded-lg p-3 bg-red-50 dark:bg-red-950/30">
      <Show when={(props.repositories?.length || 0) > 1}>
        <div class="flex items-center gap-2 mb-2">
          <select
            class="text-xs border border-gray-300 dark:border-gray-600 rounded px-1.5 py-1 bg-white dark:bg-gray-800 dark:text-gray-200 max-w-[180px]"
            value={selectedRepo()}
            onChange={(e) => {
              setSelectedRepo(e.currentTarget.value);
              setResult(null);
            }}
            disabled={props.reextracting}
          >
            <Show when={!props.currentRepo}>
              <option value="">Select repo…</option>
            </Show>
            <For each={props.repositories || []}>
              {(r) => <option value={r.id}>{r.name}</option>}
            </For>
          </select>
          <Show when={preview.loading}>
            <span class="text-xs text-gray-400">Loading counts…</span>
          </Show>
        </div>
      </Show>
      <Show when={(props.repositories?.length || 0) <= 1 && preview.loading}>
        <p class="text-xs text-gray-400 mb-2">Loading counts…</p>
      </Show>

      <Show when={preview()}>
        <div class="text-xs text-red-800 dark:text-red-300 space-y-1 mb-3">
          <p class="font-semibold">
            This will re-run concept extraction
            <Show when={(props.repositories?.length || 0) <= 1 && props.currentRepo?.name}>
              {" "}
              for <span class="font-mono">{props.currentRepo.name}</span>
            </Show>
            .
          </p>
          <ul class="list-disc list-inside space-y-0.5 pl-1">
            <li>
              <span class="font-mono font-bold">
                {preview().unlinked_stable_facts.toLocaleString()}
              </span>{" "}
              unlinked stable fact(s) will be re-attempted
            </li>
            <li>
              <span class="font-mono font-bold">{preview().retryable_skips.toLocaleString()}</span>{" "}
              retryable skip row(s) will be cleared
            </li>
            <li>
              <span class="font-mono font-bold">
                {preview().unresolved_candidates.toLocaleString()}
              </span>{" "}
              unresolved concept candidate(s) will be cleared
            </li>
            <li>
              <span class="font-mono font-bold">{preview().source_count}</span> source job(s) will
              be enqueued (one per source)
            </li>
          </ul>
          <p class="text-red-600 dark:text-red-400 italic">
            This spends LLM quota. Confirm only if you intend to re-extract.
          </p>
        </div>
      </Show>

      <div class="flex items-center gap-2">
        <Button
          variant="danger"
          onClick={handleConfirm}
          loading={props.reextracting}
          loadingText="Enqueuing..."
          disabled={!repoID() || preview.loading}
        >
          Confirm re-extract
        </Button>
        <Button variant="secondary" onClick={props.onCancel} disabled={props.reextracting}>
          Cancel
        </Button>
      </div>

      <Show when={result()}>
        <div class="mt-2 text-xs text-green-700 dark:text-green-400">
          Re-extract enqueued: {result().enqueued_job_count} job(s) (
          {result().cleared_skips.toLocaleString()} skip(s) cleared,{" "}
          {result().cleared_candidates.toLocaleString()} candidate(s) cleared).
        </div>
      </Show>
    </div>
  );
}
