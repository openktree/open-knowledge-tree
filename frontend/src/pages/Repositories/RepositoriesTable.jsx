import { Show, For, createSignal } from "solid-js";
import { useNavigate } from "@solidjs/router";
import { api } from "../../services/api";
import Button from "../../components/Button";
import Badge from "../../components/Badge";
import Card from "../../components/Card";
import FormField from "../../components/FormField";
import Alert from "../../components/Alert";

/**
 * Table of repositories the current user can see, with select, edit
 * and delete actions. The "current" row is highlighted to make the
 * active scope obvious.
 *
 * Props:
 *   - repositories: accessor () => Array<Repo>
 *   - currentRepo:  accessor () => Repo | null
 *   - onSelect:     (repo) => void
 *   - onUpdated:    () => void   — called after a successful PUT
 *   - onDeleted:    () => void   — called after a successful DELETE
 *   - onAlert:      (alert) => void
 */
export default function RepositoriesTable(props) {
  const repos = () => props.repositories() || [];
  const currentID = () => (props.currentRepo() ? props.currentRepo().id : "");
  const navigate = useNavigate();

  return (
    <Card>
      <div class="flex items-center justify-between mb-4">
        <h2 class="text-lg font-semibold dark:text-white">Your Repositories</h2>
        <span class="text-xs text-gray-500 dark:text-gray-400">
          {repos().length} total
        </span>
      </div>

      <Show
        when={repos().length > 0}
        fallback={
          <p class="text-sm text-gray-500 dark:text-gray-400">
            Nothing here yet. Use the form above to create your first repository.
          </p>
        }
      >
        <div class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="text-left border-b dark:border-gray-700">
                <th class="py-3 px-4 font-medium text-gray-600 dark:text-gray-400">Name</th>
                <th class="py-3 px-4 font-medium text-gray-600 dark:text-gray-400">Slug</th>
                <th class="py-3 px-4 font-medium text-gray-600 dark:text-gray-400">Roles</th>
                <th class="py-3 px-4 font-medium text-gray-600 dark:text-gray-400 text-right">
                  Actions
                </th>
              </tr>
            </thead>
            <tbody>
              <For each={repos()}>
                {(r) => (
                  <RepositoryRow
                    repo={r}
                    isCurrent={r.id === currentID()}
                    onSelect={() => props.onSelect?.(r)}
                    onUpdated={props.onUpdated}
                    onDeleted={props.onDeleted}
                    onAlert={props.onAlert}
                    onSettings={() => navigate(`/repositories/${r.id}/settings`)}
                  />
                )}
              </For>
            </tbody>
          </table>
        </div>
      </Show>
    </Card>
  );
}

function RepositoryRow(props) {
  const [editing, setEditing] = createSignal(false);
  const [confirmDelete, setConfirmDelete] = createSignal(false);
  const [name, setName] = createSignal(props.repo.name);
  const [description, setDescription] = createSignal(props.repo.description || "");
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal("");
  const canManage = () => (props.repo.roles || []).some((r) => r === "repoadmin" || r === "sysadmin");

  const onSave = async (e) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.updateRepository(props.repo.id, {
        name: name().trim(),
        description: description().trim(),
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
      await api.deleteRepository(props.repo.id);
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
              <FormField
                label="Name"
                value={name()}
                onChange={setName}
                required
              />
              <FormField
                label="Description"
                value={description()}
                onChange={setDescription}
              />
              <Alert variant="error" message={error()} onDismiss={() => setError("")} />
              <div class="flex gap-2">
                <Button type="submit" disabled={busy()} loading={busy()}>
                  Save
                </Button>
                <Button variant="secondary" onClick={() => setEditing(false)} disabled={busy()}>
                  Cancel
                </Button>
              </div>
            </form>
          }
        >
          <div class="flex items-center gap-2">
            <span class="font-medium dark:text-gray-200">{props.repo.name}</span>
            <Show when={props.isCurrent}>
              <Badge variant="blue">current</Badge>
            </Show>
          </div>
          <Show when={props.repo.description}>
            <p class="text-xs text-gray-500 dark:text-gray-400 mt-1">
              {props.repo.description}
            </p>
          </Show>
        </Show>
      </td>
      <td class="py-3 px-4 align-top font-mono text-xs text-gray-600 dark:text-gray-400">
        {props.repo.slug}
      </td>
      <td class="py-3 px-4 align-top">
        <div class="flex flex-wrap gap-1">
          <For each={props.repo.roles || []}>
            {(role) => (
              <Badge variant={role === "repoadmin" ? "purple" : role === "sysadmin" ? "red" : "blue"}>{role}</Badge>
            )}
          </For>
        </div>
      </td>
      <td class="py-3 px-4 align-top text-right">
        <div class="flex justify-end gap-1">
          <Show when={!props.isCurrent}>
            <Button variant="secondary" onClick={() => props.onSelect?.(props.repo)}>
              Select
            </Button>
          </Show>
          <Show when={!editing()}>
            <Button variant="secondary" onClick={() => setEditing(true)}>
              Edit
            </Button>
          </Show>
          <Show when={canManage()}>
            <Button variant="secondary" onClick={() => props.onSettings?.()}>
              Settings
            </Button>
          </Show>
          <Show
            when={!confirmDelete()}
            fallback={
              <span class="inline-flex gap-1">
                <Button
                  variant="danger"
                  onClick={onDelete}
                  disabled={busy()}
                  loading={busy()}
                >
                  Confirm delete
                </Button>
                <Button
                  variant="secondary"
                  onClick={() => setConfirmDelete(false)}
                  disabled={busy()}
                >
                  Cancel
                </Button>
              </span>
            }
          >
            <Button variant="danger" onClick={() => setConfirmDelete(true)}>
              Delete
            </Button>
          </Show>
        </div>
      </td>
    </tr>
  );
}
