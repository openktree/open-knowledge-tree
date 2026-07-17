import { createResource, createSignal, Show } from "solid-js";
import { api } from "../../services/api";
import Alert from "../../components/Alert";
import Layout from "../../components/Layout";
import AssignRoleForm from "./AssignRoleForm";
import AvailablePermissions from "./AvailablePermissions";
import UsersTable from "./UsersTable";

export default function Users() {
  const [users, { refetch }] = createResource(
    () => api.listUsers().catch(() => ({ users: [], available_permissions: [] }))
  );
  const [repositories] = createResource(
    () => api.listRepositories().catch(() => ({ repositories: [] }))
  );
  const [alert, setAlert] = createSignal(null);

  const data = () => users() || { users: [], available_permissions: [] };
  const repos = () => repositories()?.repositories || [];

  const handleAssignRole = (payload) => api.assignRole(payload);
  const handleRemoveRole = async (userId, role, repoId) => {
    try {
      await api.removeRole({ user_id: userId, role, repository_id: repoId });
      setAlert({ variant: "success", message: "Role removed" });
      refetch();
    } catch (err) {
      setAlert({ variant: "error", message: err.message });
    }
  };

  return (
    <Layout>
      <div class="space-y-6">
        <Alert
          variant={alert()?.variant}
          message={alert()?.message}
          onDismiss={() => setAlert(null)}
        />

        <AssignRoleForm
          users={() => data().users}
          repositories={repos}
          onAssign={async (payload) => {
            await handleAssignRole(payload);
            refetch();
          }}
          onAlert={setAlert}
        />

        <UsersTable
          users={() => data().users}
          onRemoveRole={handleRemoveRole}
        />

        <AvailablePermissions permissions={() => data().available_permissions} />
      </div>
    </Layout>
  );
}
