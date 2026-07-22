import { createResource, createSignal, For, Show } from "solid-js";
import Button from "../../components/Button";
import { api } from "../../services/api";

// RecomputeDangerBox is the red-bordered confirmation panel that
// appears when the user clicks "Recompute concept groups" on the
// Tasks page. It fetches a live preview (GET /concepts/recompute) of
// the current concept_groups row count + the repo's concept row
// count, shows the counts in a danger-styled box, and lets the user
// confirm or cancel. On confirm it calls the POST endpoint via
// props.onConfirm.
//
// Props:
//   repositories  — array of {id, name} for the repo dropdown
//   currentRepo   — the currently selected repo object (optional preselect)
//   recomputing   — boolean signal from the parent (POST in flight)
//   onConfirm     — async fn(repoID) → result object (called on Confirm)
export default function RecomputeDangerBox(props) {
  const [selectedRepo, setSelectedRepo] = createSignal(props.currentRepo?.id || "");
  const [result, setResult] = createSignal(null);

  const repoID = () => selectedRepo() || props.currentRepo?.id || "";

  const [preview] = createResource(repoID, async (id) => {
    if (!id) return null;
    try {
      return await api.previewRecomputeRepoConceptGroups(id);
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
    <div class="border-2 border-amber-400 dark:border-amber-600 rounded-lg p-3 bg-amber-50 dark:bg-amber-950/30">
      <Show when={(props.repositories?.length || 0) > 1}>
        <div class="flex items-center gap-2 mb-2">
          <select
            class="text-xs border border-gray-300 dark:border-gray-600 rounded px-1.5 py-1 bg-white dark:bg-gray-800 dark:text-gray-200 max-w-[180px]"
            value={selectedRepo()}
            onChange={(e) => {
              setSelectedRepo(e.currentTarget.value);
              setResult(null);
            }}
            disabled={props.recomputing}
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
        <div class="text-xs text-amber-800 dark:text-amber-300 space-y-1 mb-3">
          <p class="font-semibold">
            This will recompute the concept groups summary
            <Show when={(props.repositories?.length || 0) <= 1 && props.currentRepo?.name}>
              {" "}
              for <span class="font-mono">{props.currentRepo.name}</span>
            </Show>
            .
          </p>
          <ul class="list-disc list-inside space-y-0.5 pl-1">
            <li>
              <span class="font-mono font-bold">{preview().current_groups.toLocaleString()}</span>{" "}
              group row(s) currently in the summary
            </li>
            <li>
              <span class="font-mono font-bold">{preview().concepts_total.toLocaleString()}</span>{" "}
              concept row(s) will be re-aggregated from
            </li>
          </ul>
          <p class="text-amber-700 dark:text-amber-400 italic">
            This is the repair path; the summary is kept live by the ingest workers otherwise.
          </p>
        </div>
      </Show>

      <div class="flex items-center gap-2">
        <Button
          variant="danger"
          onClick={handleConfirm}
          loading={props.recomputing}
          loadingText="Enqueuing..."
          disabled={!repoID() || preview.loading}
        >
          Confirm recompute
        </Button>
        <Button variant="secondary" onClick={props.onCancel} disabled={props.recomputing}>
          Cancel
        </Button>
      </div>

      <Show when={result()}>
        <div class="mt-2 text-xs text-green-700 dark:text-green-400">
          Recompute enqueued — see job <span class="font-mono">{result().enqueued_job_id}</span> in
          the table above.
        </div>
      </Show>
    </div>
  );
}
