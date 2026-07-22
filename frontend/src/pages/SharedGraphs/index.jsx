import { createResource, createSignal, Show } from "solid-js";
import Alert from "../../components/Alert";
import Layout from "../../components/Layout";
import { api } from "../../services/api";
import { useRBAC } from "../../store/rbac";
import { useRepository } from "../../store/repository";
import ImportGraphDialog from "./ImportGraphDialog";
import SharedGraphsContent from "./SharedGraphsContent";

const PAGE_SIZE = 20;

export default function SharedGraphs() {
  const rbac = useRBAC();
  const repo = useRepository();
  const canImport = () => rbac.hasPermission("graph", "write");

  const [alert, setAlert] = createSignal(null);
  const [search, setSearch] = createSignal("");
  const [tag, setTag] = createSignal("");
  const [offset, setOffset] = createSignal(0);
  const [selectedGraph, setSelectedGraph] = createSignal(null);
  const [uploadMode, setUploadMode] = createSignal(false);

  const currentRepoSlug = () => (repo.currentRepo() ? repo.currentRepo().slug : "");

  const [graphs, { refetch }] = createResource(
    () => ({ q: search(), tag: tag(), offset: offset() }),
    async (params) => {
      try {
        return await api.listSharedGraphs({
          q: params.q,
          tag: params.tag,
          limit: PAGE_SIZE,
          offset: params.offset,
        });
      } catch (err) {
        setAlert({ variant: "error", message: err.message });
        return null;
      }
    },
  );

  const onSearch = (q) => {
    setSearch(q);
    setOffset(0);
  };
  const onTagChange = (t) => {
    setTag(t);
    setOffset(0);
  };

  const handleImport = (g) => {
    setSelectedGraph(g);
    setUploadMode(false);
  };
  const handleUpload = () => {
    setSelectedGraph({});
    setUploadMode(true);
  };
  const handleCloseDialog = () => {
    setSelectedGraph(null);
    setUploadMode(false);
  };
  const handleImportSuccess = (info) => {
    setSelectedGraph(null);
    setUploadMode(false);
    setAlert({ variant: "success", message: info.message });
  };

  return (
    <Layout>
      <Show
        when={canImport()}
        fallback={
          <p class="text-sm text-text-muted">
            You do not have permission to import shared graphs. Ask a repository admin to grant you
            the graph:write permission.
          </p>
        }
      >
        <div>
          <Alert
            variant={alert()?.variant}
            message={alert()?.message}
            onDismiss={() => setAlert(null)}
          />
          <SharedGraphsContent
            graphs={graphs}
            loading={() => graphs.loading}
            search={search}
            tag={tag}
            onSearch={onSearch}
            onTagChange={onTagChange}
            offset={offset}
            onOffsetChange={setOffset}
            total={() => graphs()?.total || 0}
            limit={PAGE_SIZE}
            onOpenDetail={handleImport}
            onImport={handleImport}
            onUpload={handleUpload}
          />
        </div>
        <Show when={selectedGraph()}>
          <ImportGraphDialog
            graph={selectedGraph()}
            upload={uploadMode()}
            currentRepoSlug={currentRepoSlug}
            onClose={handleCloseDialog}
            onSuccess={handleImportSuccess}
          />
        </Show>
      </Show>
    </Layout>
  );
}
