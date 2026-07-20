// @okt-page-allow-large: folder page; checker miscounts default export as internal subcomponent
import { createMemo, createResource, createSignal, Show } from "solid-js";
import Alert from "../../components/Alert";
import Button from "../../components/Button";
import EmptyState from "../../components/EmptyState";
import Layout from "../../components/Layout";
import Pagination from "../../components/Pagination";
import UploadSourcePanel from "../../components/UploadSourcePanel";
import { api } from "../../services/api";
import { useRBAC } from "../../store/rbac";
import { useRepository } from "../../store/repository";
import SourcesForm from "./SourcesForm";
import SourcesList from "./SourcesList";

const PAGE_SIZE = 100;

export default function Sources() {
  const rbac = useRBAC();
  const repo = useRepository();
  const canReadSources = createMemo(() => rbac.hasPermission("source", "read"));
  const canCreateSources = createMemo(() => rbac.hasPermission("source", "write"));
  const canDeleteSources = createMemo(() => rbac.hasPermission("source", "delete"));
  const canUpdateSources = createMemo(() => rbac.hasPermission("source", "update"));

  const [alert, setAlert] = createSignal(null);
  const [addURL, setAddURL] = createSignal("");
  const [addKind, setAddKind] = createSignal("homepage");
  const [creating, setCreating] = createSignal(false);
  const [deletingID, setDeletingID] = createSignal("");
  const [processingID, setProcessingID] = createSignal("");
  const [retryingID, setRetryingID] = createSignal("");
  const [search, setSearch] = createSignal("");
  const [offset, setOffset] = createSignal(0);
  const [showAdd, setShowAdd] = createSignal(null);

  const [sources, { refetch, mutate }] = createResource(
    () => ({
      slug: repo.currentRepo() ? repo.currentRepo().slug : "",
      q: search(),
      offset: offset(),
    }),
    async ({ slug, q, offset }) => {
      if (!slug) return null;
      try {
        const data = await api.listSources(slug, { q, limit: PAGE_SIZE, offset });
        return data;
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

  const handleDelete = async (source) => {
    const slug = repo.currentRepo()?.slug;
    if (!slug) return;
    if (!window.confirm(`Delete source "${source.url}"? This cannot be undone.`)) return;
    setDeletingID(source.id);
    setAlert(null);
    try {
      await api.deleteSource(slug, source.id);
      refetch();
    } catch (err) {
      setAlert({ variant: "error", message: err.message });
    } finally {
      setDeletingID("");
    }
  };

  const handleProcess = async (source) => {
    const slug = repo.currentRepo()?.slug;
    if (!slug) return;
    setProcessingID(source.id);
    setAlert(null);
    try {
      await api.processSource(slug, source.id);
      setAlert({
        variant: "success",
        message: "Decomposition job queued for " + (source.parsed_title || source.url),
      });
      mutate((current) =>
        current
          ? {
              ...current,
              data: current.data.map((s) =>
                s.id === source.id ? { ...s, status: "processed" } : s,
              ),
            }
          : current,
      );
    } catch (err) {
      setAlert({ variant: "error", message: err.message });
    } finally {
      setProcessingID("");
    }
  };

  const handleRetry = async (source) => {
    const slug = repo.currentRepo()?.slug;
    if (!slug) return;
    setRetryingID(source.id);
    setAlert(null);
    try {
      await api.retrySource(slug, source.id);
      setAlert({
        variant: "success",
        message: "Re-queued fetch for " + (source.parsed_title || source.url),
      });
      // Optimistically flip the row back to 'pending' so the UI
      // reflects the reset before the worker picks the job up.
      mutate((current) =>
        current
          ? {
              ...current,
              data: current.data.map((s) =>
                s.id === source.id ? { ...s, status: "pending", error: null } : s,
              ),
            }
          : current,
      );
    } catch (err) {
      setAlert({ variant: "error", message: err.message });
    } finally {
      setRetryingID("");
    }
  };

  const handleAdd = async (e) => {
    e.preventDefault();
    const url = addURL().trim();
    if (!url) return;
    const slug = repo.currentRepo()?.slug;
    if (!slug) return;
    setCreating(true);
    setAlert(null);
    try {
      // Detect a bare DOI ("10.…" or "doi.org/…") so the worker
      // takes the Unpaywall OA path first. Everything else is
      // treated as a URL.
      let doi = "";
      const bare = url.replace(/^https?:\/\/(dx\.)?doi\.org\//i, "");
      if (/^10\./.test(bare)) {
        doi = bare;
      }
      await api.retrieveSource(url, repo.currentRepo().id, true, doi);
      setAddURL("");
      setAddKind("homepage");
      setAlert({ variant: "success", message: "Source queued for fetch + decomposition." });
      // The retrieve_source worker writes the row asynchronously;
      // refetch after a beat so the new row appears in the list.
      setTimeout(() => refetch(), 800);
    } catch (err) {
      setAlert({ variant: "error", message: err.message });
    } finally {
      setCreating(false);
    }
  };

  return (
    <Layout>
      <Show
        when={canReadSources()}
        fallback={
          <EmptyState
            title="You do not have permission to view sources."
            description="Ask a repository admin to grant you the source:read permission."
          />
        }
      >
        <div>
          <Alert
            variant={alert()?.variant}
            message={alert()?.message}
            onDismiss={() => setAlert(null)}
          />

          <Show when={canCreateSources()}>
            <Show
              when={showAdd()}
              fallback={
                <div class="flex gap-3 mb-6">
                  <Button onClick={() => setShowAdd("url")}>Add URL</Button>
                  <Button onClick={() => setShowAdd("upload")}>Upload Source</Button>
                </div>
              }
            >
              <Show when={showAdd() === "url"}>
                <SourcesForm
                  canCreate={canCreateSources()}
                  addURL={addURL}
                  onChangeURL={setAddURL}
                  addKind={addKind}
                  onChangeKind={setAddKind}
                  creating={creating}
                  onAdd={handleAdd}
                  onCancel={() => setShowAdd(null)}
                />
              </Show>

              <Show when={showAdd() === "upload" && repo.currentRepo()}>
                <UploadSourcePanel
                  slug={repo.currentRepo().slug}
                  onDone={() => setTimeout(() => refetch(), 800)}
                  onCancel={() => setShowAdd(null)}
                />
              </Show>
            </Show>
          </Show>

          <SourcesList
            hasRepo={() => !!repo.currentRepo()}
            slug={() => repo.currentRepo()?.slug}
            sources={sources}
            loading={() => sources.loading}
            canProcess={canUpdateSources}
            processingID={processingID}
            onProcess={handleProcess}
            canRetry={canUpdateSources}
            retryingID={retryingID}
            onRetry={handleRetry}
            canDelete={canDeleteSources}
            deletingID={deletingID}
            onDelete={handleDelete}
            onRefresh={refetch}
            search={search}
            onSearch={onSearch}
            offset={offset}
            onOffsetChange={setOffset}
            total={() => sources()?.total || 0}
            limit={PAGE_SIZE}
          />
        </div>
      </Show>
    </Layout>
  );
}
