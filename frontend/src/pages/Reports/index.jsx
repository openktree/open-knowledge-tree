// @okt-page-allow-large: folder page; checker miscounts default export as internal subcomponent
import { createResource, createSignal, Show } from "solid-js";
import Alert from "../../components/Alert";
import Button from "../../components/Button";
import EmptyState from "../../components/EmptyState";
import Layout from "../../components/Layout";
import Loading from "../../components/Loading";
import { api } from "../../services/api";
import { useRBAC } from "../../store/rbac";
import { useRepository } from "../../store/repository";
import CreateReportForm from "./CreateReportForm";
import ReportsTable from "./ReportsTable";

export default function Reports() {
  const repo = useRepository();
  const rbac = useRBAC();
  const [alert, setAlert] = createSignal(null);
  const [refreshKey, setRefreshKey] = createSignal(0);
  const [showCreate, setShowCreate] = createSignal(false);

  const canCreate = () => rbac.hasPermission("report", "write");
  const canRead = () => rbac.hasPermission("report", "read");
  const slug = () => repo.currentRepo()?.slug || "";

  const [reportsData] = createResource(
    () => [repo.loaded(), slug(), refreshKey()],
    async ([_loaded, s]) => {
      if (!s) return { data: [], total: 0, limit: 100, offset: 0 };
      try {
        return await api.listReports(s);
      } catch (err) {
        setAlert({ variant: "error", message: err.message });
        return { data: [], total: 0, limit: 100, offset: 0 };
      }
    },
  );

  const reports = () => reportsData()?.data || [];

  return (
    <Layout>
      <Show when={repo.loaded()} fallback={<Loading message="Loading repository..." />}>
        <Show
          when={slug()}
          fallback={
            <EmptyState
              title="No repository selected"
              description="Select a repository to view reports."
            />
          }
        >
          <Show
            when={canRead()}
            fallback={
              <EmptyState
                title="Permission required"
                description="You do not have permission to read reports."
              />
            }
          >
            <div class="space-y-6">
              <Show when={alert()}>
                <Alert
                  variant={alert()?.variant}
                  message={alert()?.message}
                  onDismiss={() => setAlert(null)}
                />
              </Show>
              <Show when={canCreate()}>
                <Show
                  when={showCreate()}
                  fallback={<Button onClick={() => setShowCreate(true)}>New Report</Button>}
                >
                  <CreateReportForm
                    slug={slug()}
                    parentOptions={reports().filter((r) => !r.parent_id)}
                    onCreated={() => {
                      setRefreshKey((k) => k + 1);
                      setShowCreate(false);
                    }}
                    onAlert={setAlert}
                    onCancel={() => setShowCreate(false)}
                  />
                </Show>
              </Show>
              <Show when={!reportsData.loading} fallback={<Loading message="Loading reports..." />}>
                <ReportsTable
                  slug={slug()}
                  reports={reports}
                  onDeleted={() => setRefreshKey((k) => k + 1)}
                  onAlert={setAlert}
                />
              </Show>
            </div>
          </Show>
        </Show>
      </Show>
    </Layout>
  );
}
