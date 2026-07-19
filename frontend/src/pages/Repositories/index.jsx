import { createResource, createSignal, Show } from "solid-js";
import Alert from "../../components/Alert";
import EmptyState from "../../components/EmptyState";
import Layout from "../../components/Layout";
import Loading from "../../components/Loading";
import { api } from "../../services/api";
import { useRepository } from "../../store/repository";
import CreateRepositoryForm from "./CreateRepositoryForm";
import CurrentRepositoryCard from "./CurrentRepositoryCard";
import RepositoriesTable from "./RepositoriesTable";
import RepositoryDetails from "./RepositoryDetails";

/**
 * Repositories management page.
 *
 * The repository is the top-level scope for all data in the
 * application, so this page is the entry point for users without any
 * repository yet, and the management surface for users that already
 * have one or more.
 */
export default function Repositories() {
  const repo = useRepository();
  const [alert, setAlert] = createSignal(null);

  const [repos] = createResource(
    () => [repo.loaded(), repo.repositories()],
    () => api.listRepositories().catch(() => ({ repositories: [] })),
  );

  const data = () => repos() || { repositories: [] };

  return (
    <Layout>
      <div class="space-y-6">
        <Alert
          variant={alert()?.variant}
          message={alert()?.message}
          onDismiss={() => setAlert(null)}
        />

        <Show when={repo.loaded()} fallback={<Loading message="Loading repositories..." />}>
          <CurrentRepositoryCard onAlert={setAlert} />

          <Show when={data().repositories.length === 0}>
            <EmptyState
              title="No repositories yet"
              description="Create your first repository below — every other piece of data in Open Knowledge Tree lives inside a repository."
            />
          </Show>

          <CreateRepositoryForm
            onCreated={async () => {
              await repo.refresh();
              setAlert({ variant: "success", message: "Repository created" });
            }}
            onAlert={setAlert}
          />

          <RepositoriesTable
            repositories={() => data().repositories}
            currentRepo={repo.currentRepo}
            onSelect={(r) => {
              repo.selectRepository(r);
              setAlert({ variant: "success", message: `Switched to ${r.name}` });
            }}
            onUpdated={async () => {
              await repo.refresh();
              setAlert({ variant: "success", message: "Repository updated" });
            }}
            onDeleted={async () => {
              await repo.refresh();
              setAlert({ variant: "success", message: "Repository deleted" });
            }}
            onAlert={setAlert}
          />

          <RepositoryDetails />
        </Show>
      </div>
    </Layout>
  );
}
