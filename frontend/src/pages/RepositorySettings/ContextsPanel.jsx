import { createSignal, For, Show } from "solid-js";
import Badge from "../../components/Badge";
import Button from "../../components/Button";
import FormField from "../../components/FormField";
import { api } from "../../services/api";
import MigrateContextDialog from "./MigrateContextDialog";

// ContextsPanel renders the repo's allowed context list with
// add/edit/migrate/delete actions. Delete is blocked while a
// context still has concepts (the server returns 409); the UI
// shows the concept_count and requires a migrate first.
//
// Props:
//   - repoID:    () => string
//   - contexts:  () => Array<{context,is_custom,description,concept_count}>
//   - onChanged: () => void  — refetch settings after a mutation
//   - onAlert:   (alert) => void
export default function ContextsPanel(props) {
  const [adding, setAdding] = createSignal(false);
  const [newContext, setNewContext] = createSignal("");
  const [newDesc, setNewDesc] = createSignal("");
  const [busy, setBusy] = createSignal(false);
  const [migrating, setMigrating] = createSignal(null);
  const [editing, setEditing] = createSignal("");
  const [editDesc, setEditDesc] = createSignal("");

  const addContext = async (e) => {
    e.preventDefault();
    if (!newContext().trim()) return;
    setBusy(true);
    try {
      await api.addRepositoryContext(props.repoID(), {
        context: newContext().trim(),
        description: newDesc().trim(),
      });
      setNewContext("");
      setNewDesc("");
      setAdding(false);
      props.onChanged?.();
    } catch (err) {
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setBusy(false);
    }
  };

  const saveEdit = async (ctx) => {
    setBusy(true);
    try {
      await api.updateRepositoryContext(props.repoID(), ctx, { description: editDesc().trim() });
      setEditing("");
      props.onChanged?.();
    } catch (err) {
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setBusy(false);
    }
  };

  const del = async (ctx) => {
    if (!confirm(`Delete context "${ctx}"? This removes it from the allowed list.`)) return;
    setBusy(true);
    try {
      await api.deleteRepositoryContext(props.repoID(), ctx);
      props.onChanged?.();
    } catch (err) {
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="space-y-4">
      <div class="flex items-center justify-between">
        <h3 class="text-lg font-semibold dark:text-white">Concept Contexts</h3>
        <Button variant="secondary" onClick={() => setAdding(!adding())}>
          {adding() ? "Cancel" : "Add context"}
        </Button>
      </div>
      <p class="text-sm text-gray-500 dark:text-gray-400">
        The allowed context (ontology class) labels the concept-extraction prompt may assign.
        DBpedia-derived labels are seeded at creation; custom labels (Product, Application, Role, …)
        are admin-defined.
      </p>
      <Show when={adding()}>
        <form
          onSubmit={addContext}
          class="space-y-2 p-3 rounded border border-gray-200 dark:border-gray-700"
        >
          <FormField
            label="Context label"
            value={newContext()}
            onChange={setNewContext}
            placeholder="e.g. Product"
            required
          />
          <FormField
            label="Description"
            value={newDesc()}
            onChange={setNewDesc}
            placeholder="Short description (optional)"
          />
          <Button type="submit" disabled={busy() || !newContext().trim()} loading={busy()}>
            Add
          </Button>
        </form>
      </Show>
      <ul class="divide-y dark:divide-gray-700">
        <For each={props.contexts() || []}>
          {(c) => (
            <li class="py-3">
              <div class="flex items-start justify-between gap-2">
                <div class="min-w-0 flex-1">
                  <div class="flex items-center gap-2">
                    <span class="font-medium dark:text-gray-200">{c.context}</span>
                    <Show when={c.is_custom}>
                      <Badge variant="purple">custom</Badge>
                    </Show>
                    <Show when={c.concept_count > 0}>
                      <Badge variant="blue">{c.concept_count} concepts</Badge>
                    </Show>
                  </div>
                  <Show when={editing() === c.context}>
                    <div class="mt-2 space-y-2">
                      <FormField label="Description" value={editDesc()} onChange={setEditDesc} />
                      <div class="flex gap-2">
                        <Button
                          onClick={() => saveEdit(c.context)}
                          disabled={busy()}
                          loading={busy()}
                        >
                          Save
                        </Button>
                        <Button
                          variant="secondary"
                          onClick={() => setEditing("")}
                          disabled={busy()}
                        >
                          Cancel
                        </Button>
                      </div>
                    </div>
                  </Show>
                  <Show when={editing() !== c.context && c.description}>
                    <p class="text-xs text-gray-500 dark:text-gray-400 mt-1">{c.description}</p>
                  </Show>
                </div>
                <div class="flex flex-wrap justify-end gap-1">
                  <Button
                    variant="secondary"
                    onClick={() => {
                      setEditing(c.context);
                      setEditDesc(c.description || "");
                    }}
                  >
                    Edit
                  </Button>
                  <Button
                    variant="secondary"
                    onClick={() => setMigrating(c.context)}
                    disabled={c.concept_count === 0 && true}
                  >
                    Migrate
                  </Button>
                  <Button variant="danger" onClick={() => del(c.context)} disabled={busy()}>
                    Delete
                  </Button>
                </div>
              </div>
            </li>
          )}
        </For>
      </ul>
      <Show when={migrating()}>
        <MigrateContextDialog
          repoID={props.repoID}
          contexts={props.contexts}
          sourceContext={migrating()}
          onClose={() => setMigrating(null)}
          onMigrated={() => {
            setMigrating(null);
            props.onChanged?.();
          }}
          onAlert={props.onAlert}
        />
      </Show>
    </div>
  );
}
