import { createSignal, For } from "solid-js";
import Alert from "../../components/Alert";
import Button from "../../components/Button";
import Card from "../../components/Card";
import FormField from "../../components/FormField";
import { ALL_REPOSITORIES, ASSIGNABLE_ROLES } from "./constants";

/**
 * Form to assign a role to a user on a repository.
 *
 * Props:
 *   - users:            accessor () => Array<User> (only need id, display_name, email)
 *   - repositories:     accessor () => Array<Repo> (only need id, name)
 *   - onAssigned:       () => void   — called after a successful assign (parent refetches)
 *   - onAlert:          (alert) => void — { variant, message } | null
 *   - usersWithRolesFilter: optional accessor () => Array<User>; if provided,
 *                          only users that currently have at least one role are listed.
 *                          Default: show all users.
 */
export default function AssignRoleForm(props) {
  const [selectedUser, setSelectedUser] = createSignal(null);
  const [selectedRole, setSelectedRole] = createSignal(
    ASSIGNABLE_ROLES[ASSIGNABLE_ROLES.length - 1].value,
  );
  const [selectedRepo, setSelectedRepo] = createSignal(ALL_REPOSITORIES);
  const [submitting, setSubmitting] = createSignal(false);
  const [localError, setLocalError] = createSignal("");

  const candidateUsers = () => {
    const all = props.users() || [];
    return props.usersWithRolesFilter ? (props.usersWithRolesFilter() ?? all) : all;
  };

  const handleSubmit = async (e) => {
    e.preventDefault();
    const user = selectedUser();
    if (!user) {
      setLocalError("Please select a user");
      return;
    }
    setSubmitting(true);
    try {
      // Parent provides the assign call so the form stays free of api imports.
      // The contract: parent passes a function via props.onAssign(payload) -> Promise.
      await props.onAssign({
        user_id: user.id,
        role: selectedRole(),
        repository_id: selectedRepo(),
      });
      props.onAlert?.({
        variant: "success",
        message: `Role ${selectedRole()} assigned to ${user.display_name || user.email}`,
      });
      setSelectedUser(null);
    } catch (err) {
      setLocalError(err.message);
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Card>
      <h2 class="text-lg font-semibold mb-4 dark:text-white">Assign Role</h2>
      <form onSubmit={handleSubmit} class="flex gap-3 items-end">
        <FormField
          class="flex-1"
          label="User"
          type="select"
          value={selectedUser()?.id || ""}
          onChange={(id) => {
            const user = candidateUsers().find((u) => u.id === id);
            setSelectedUser(user || null);
          }}
        >
          <option value="">Select user...</option>
          <For each={candidateUsers()}>
            {(u) => (
              <option value={u.id}>
                {u.display_name || u.email} ({u.email})
              </option>
            )}
          </For>
        </FormField>
        <FormField label="Role" type="select" value={selectedRole()} onChange={setSelectedRole}>
          <For each={ASSIGNABLE_ROLES}>{(r) => <option value={r.value}>{r.label}</option>}</For>
        </FormField>
        <FormField
          label="Repository"
          type="select"
          value={selectedRepo()}
          onChange={setSelectedRepo}
          class="w-48"
        >
          <option value={ALL_REPOSITORIES}>All Repositories</option>
          <For each={props.repositories?.() || []}>
            {(repo) => <option value={repo.id}>{repo.name}</option>}
          </For>
        </FormField>
        <Button type="submit" disabled={!selectedUser() || submitting()} class="h-10">
          Assign
        </Button>
      </form>
      <Alert variant="error" message={localError()} onDismiss={() => setLocalError("")} />
    </Card>
  );
}
