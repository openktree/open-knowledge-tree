import { Show, createSignal } from "solid-js";
import { A } from "@solidjs/router";
import { api } from "../../services/api";
import Button from "../../components/Button";
import FormField from "../../components/FormField";
import Alert from "../../components/Alert";

export default function InvestigationRow(props) {
  const [editing, setEditing] = createSignal(false);
  const [confirmDelete, setConfirmDelete] = createSignal(false);
  const [title, setTitle] = createSignal(props.inv.title);
  const [topic, setTopic] = createSignal(props.inv.topic || "");
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal("");

  const created = () =>
    props.inv.created_at
      ? new Date(props.inv.created_at).toLocaleDateString()
      : "\u2014";

  const href = () => `/${props.slug}/investigations/${props.inv.id}`;

  const onSave = async (e) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.updateInvestigation(props.slug, props.inv.id, {
        title: title().trim(),
        topic: topic().trim(),
      });
      setEditing(false);
      props.onUpdated?.();
    } catch (err) {
      setError(err.message);
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setBusy(false);
    }
  };

  const onDelete = async () => {
    setBusy(true);
    setError("");
    try {
      await api.deleteInvestigation(props.slug, props.inv.id);
      setConfirmDelete(false);
      props.onDeleted?.();
    } catch (err) {
      setError(err.message);
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setBusy(false);
    }
  };

  return (
    <tr class="border-b dark:border-gray-700 hover:bg-gray-50 dark:hover:bg-gray-700/50">
      <td class="py-3 px-4 align-top">
        <Show
          when={!editing()}
          fallback={
            <form onSubmit={onSave} class="space-y-2">
              <FormField label="Title" value={title()} onChange={setTitle} required />
              <FormField label="Topic" value={topic()} onChange={setTopic} />
              <Alert variant="error" message={error()} onDismiss={() => setError("")} />
              <div class="flex gap-2">
                <Button type="submit" disabled={busy()} loading={busy()}>Save</Button>
                <Button variant="secondary" onClick={() => setEditing(false)} disabled={busy()}>Cancel</Button>
              </div>
            </form>
          }
        >
          <A href={href()} class="text-blue-600 dark:text-blue-400 hover:underline font-medium">
            {props.inv.title}
          </A>
        </Show>
      </td>
      <td class="py-3 px-4 align-top text-gray-600 dark:text-gray-400">
        {props.inv.topic || "\u2014"}
      </td>
      <td class="py-3 px-4 align-top text-gray-600 dark:text-gray-400">{created()}</td>
      <td class="py-3 px-4 align-top text-right">
        <div class="flex justify-end gap-1">
          <Show when={!editing()}>
            <Button variant="secondary" onClick={() => setEditing(true)}>Edit</Button>
          </Show>
          <Show
            when={!confirmDelete()}
            fallback={
              <span class="inline-flex gap-1">
                <Button variant="danger" onClick={onDelete} disabled={busy()} loading={busy()}>
                  Confirm delete
                </Button>
                <Button variant="secondary" onClick={() => setConfirmDelete(false)} disabled={busy()}>
                  Cancel
                </Button>
              </span>
            }
          >
            <Button variant="danger" onClick={() => setConfirmDelete(true)}>Delete</Button>
          </Show>
        </div>
      </td>
    </tr>
  );
}