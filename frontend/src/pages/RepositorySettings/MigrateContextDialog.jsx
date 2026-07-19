import { createSignal, For, Show } from "solid-js";
import Button from "../../components/Button";
import Modal from "../../components/Modal";
import { api } from "../../services/api";

// MigrateContextDialog lets the admin pick a target context and
// enqueues a migrate_context job. The server merges concepts under
// the source context into the target (re-linking facts + aliases,
// deleting the old rows). Polls the job status until complete.
//
// Props:
//   - repoID:        () => string
//   - contexts:      () => Array<{context, ...}>
//   - sourceContext: string
//   - onClose:       () => void
//   - onMigrated:    () => void
//   - onAlert:       (alert) => void
export default function MigrateContextDialog(props) {
  const [target, setTarget] = createSignal("");
  const [busy, setBusy] = createSignal(false);
  const [jobID, setJobID] = createSignal("");
  const [status, setStatus] = createSignal("");

  const targets = () => (props.contexts() || []).filter((c) => c.context !== props.sourceContext);

  const submit = async () => {
    if (!target()) return;
    setBusy(true);
    setStatus("queued");
    try {
      const res = await api.migrateRepositoryContext(props.repoID(), props.sourceContext, {
        target_context: target(),
      });
      setJobID(res.job_id);
      setStatus("running");
      poll();
    } catch (err) {
      props.onAlert?.({ variant: "error", message: err.message });
      setBusy(false);
    }
  };

  const poll = async () => {
    if (!jobID()) return;
    try {
      const job = await api.getTask(jobID());
      if (job.state === "completed") {
        setStatus("completed");
        setBusy(false);
        props.onAlert?.({ variant: "success", message: "Migration complete" });
        props.onMigrated?.();
      } else if (job.state === "discarded" || job.state === "cancelled") {
        setStatus(job.state);
        setBusy(false);
        props.onAlert?.({ variant: "error", message: `Migration ${job.state}` });
      } else {
        setStatus(job.state);
        setTimeout(poll, 2000);
      }
    } catch (err) {
      setBusy(false);
      props.onAlert?.({ variant: "error", message: err.message });
    }
  };

  return (
    <Modal open onClose={props.onClose} title={`Migrate "${props.sourceContext}"`}>
      <div class="space-y-4">
        <p class="text-sm text-gray-600 dark:text-gray-300">
          Re-assign every concept currently under{" "}
          <span class="font-medium">{props.sourceContext}</span> to another context. Concepts whose
          (name, target) already exists are merged (facts + aliases combined); others are
          re-pointed. Re-embedding and relations refresh run after.
        </p>
        <div>
          <label class="block mb-1 text-sm font-medium dark:text-gray-300">Target context</label>
          <select
            value={target()}
            onChange={(e) => setTarget(e.target.value)}
            class="w-full px-3 py-2 border rounded dark:bg-gray-700 dark:border-gray-600 dark:text-gray-200 border-gray-300"
          >
            <option value="">Select…</option>
            <For each={targets()}>{(c) => <option value={c.context}>{c.context}</option>}</For>
          </select>
        </div>
        <Show when={status}>
          <p class="text-xs text-gray-500 dark:text-gray-400">Status: {status()}</p>
        </Show>
        <div class="flex justify-end gap-2">
          <Button variant="secondary" onClick={props.onClose} disabled={busy()}>
            Close
          </Button>
          <Button onClick={submit} disabled={busy() || !target()} loading={busy()}>
            Migrate
          </Button>
        </div>
      </div>
    </Modal>
  );
}
