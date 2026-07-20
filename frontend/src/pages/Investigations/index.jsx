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
import RegistryBanner from "../Dashboard/RegistryBanner";
import CreateInvestigationForm from "./CreateInvestigationForm";
import InvestigationsTable from "./InvestigationsTable";

export default function Investigations() {
  const repo = useRepository();
  const rbac = useRBAC();
  const [alert, setAlert] = createSignal(null);
  const [refreshKey, setRefreshKey] = createSignal(0);
  const [showCreate, setShowCreate] = createSignal(false);

  const canCreate = () => rbac.hasPermission("investigation", "write");

  const slug = () => repo.currentRepo()?.slug || "";

  const [invData] = createResource(
    () => [repo.loaded(), slug(), refreshKey()],
    async ([_loaded, s]) => {
      if (!s) return { data: [], total: 0, limit: 100, offset: 0 };
      try {
        return await api.listInvestigations(s);
      } catch (err) {
        setAlert({ variant: "error", message: err.message });
        return { data: [], total: 0, limit: 100, offset: 0 };
      }
    },
  );

  const investigations = () => invData()?.data || [];

  return (
    <Layout>
      <div class="space-y-6">
        <Alert
          variant={alert()?.variant}
          message={alert()?.message}
          onDismiss={() => setAlert(null)}
        />
        <Show when={repo.loaded()} fallback={<Loading message="Loading repository..." />}>
          <Show
            when={slug()}
            fallback={
              <EmptyState
                title="No repository selected"
                description="Select a repository to view its investigations."
              />
            }
          >
            <RegistryBanner repoID={() => repo.currentRepo()?.id} />
            <Show
              when={!invData.loading}
              fallback={<Loading message="Loading investigations..." />}
            >
              <Show when={investigations().length === 0}>
                <EmptyState
                  title="No investigations yet"
                  description="Group sources and their facts around a research topic by creating an investigation."
                />
              </Show>
              <Show when={canCreate()}>
                <Show
                  when={showCreate()}
                  fallback={
                    <Button onClick={() => setShowCreate(true)}>Create Investigation</Button>
                  }
                >
                  <CreateInvestigationForm
                    slug={slug()}
                    onCreated={() => {
                      setRefreshKey((k) => k + 1);
                      setAlert({ variant: "success", message: "Investigation created" });
                      setShowCreate(false);
                    }}
                    onAlert={setAlert}
                    onCancel={() => setShowCreate(false)}
                  />
                </Show>
              </Show>
              <InvestigationsTable
                slug={slug()}
                investigations={investigations}
                onUpdated={() => {
                  setRefreshKey((k) => k + 1);
                  setAlert({ variant: "success", message: "Investigation updated" });
                }}
                onDeleted={() => {
                  setRefreshKey((k) => k + 1);
                  setAlert({ variant: "success", message: "Investigation deleted" });
                }}
                onAlert={setAlert}
              />
            </Show>
          </Show>
        </Show>
      </div>
    </Layout>
  );
}
