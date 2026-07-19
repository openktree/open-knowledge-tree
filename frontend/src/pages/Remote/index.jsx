// @okt-page-allow-large: page folder; default export counts as JSX subcomponent
import { createMemo, createResource, createSignal, Show } from "solid-js";
import Alert from "../../components/Alert";
import EmptyState from "../../components/EmptyState";
import Layout from "../../components/Layout";
import { api } from "../../services/api";
import { useRBAC } from "../../store/rbac";
import { useRepository } from "../../store/repository";
import RemoteContent from "./RemoteContent";

const PAGE_SIZE = 20;

export default function Remote() {
  const rbac = useRBAC();
  const repo = useRepository();
  const canRead = createMemo(() => rbac.hasPermission("remote", "read"));
  const canManage = createMemo(() => rbac.hasPermission("repository", "manage"));

  const [alert, setAlert] = createSignal(null);
  const [search, setSearch] = createSignal("");
  const [offset, setOffset] = createSignal(0);
  const [pullingID, setPullingID] = createSignal("");
  const [selectedSource, setSelectedSource] = createSignal(null);
  const [busyPushAll, setBusyPushAll] = createSignal(false);
  const [busyPullAll, setBusyPullAll] = createSignal(false);
  const [busyPullPage, setBusyPullPage] = createSignal(false);
  const [busyPullAllResults, setBusyPullAllResults] = createSignal(false);

  const slug = () => (repo.currentRepo() ? repo.currentRepo().slug : "");
  const repoID = () => (repo.currentRepo() ? repo.currentRepo().id : "");

  const [sources, { refetch }] = createResource(
    () => ({
      slug: slug(),
      q: search(),
      offset: offset(),
    }),
    async ({ slug, q, offset }) => {
      if (!slug) return null;
      try {
        return await api.listRemoteSources(slug, { q, limit: PAGE_SIZE, offset });
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

  const handlePull = async (src) => {
    const s = slug();
    if (!s) return;
    setPullingID(src.id);
    setAlert(null);
    try {
      const result = await api.pullRemoteSource(s, src.id);
      setAlert({
        variant: "success",
        message: `Pulled "${result.title || src.title || src.url}" — ${result.imported_facts} facts, ${result.imported_concepts} concepts`,
      });
      refetch();
      return result;
    } catch (err) {
      setAlert({ variant: "error", message: err.message });
      throw err;
    } finally {
      setPullingID("");
    }
  };

  const handlePushAll = async () => {
    const id = repoID();
    if (!id) return;
    setBusyPushAll(true);
    setAlert(null);
    try {
      const res = await api.contributeAll(id);
      setAlert({ variant: "success", message: `Push All enqueued (job: ${res.job_id})` });
    } catch (err) {
      setAlert({ variant: "error", message: err.message });
    } finally {
      setBusyPushAll(false);
    }
  };

  const handlePullAll = async () => {
    const id = repoID();
    if (!id) return;
    setBusyPullAll(true);
    setAlert(null);
    try {
      const res = await api.pullAllFromRegistry(id);
      setAlert({ variant: "success", message: `Pull All enqueued (job: ${res.job_id})` });
    } catch (err) {
      setAlert({ variant: "error", message: err.message });
    } finally {
      setBusyPullAll(false);
    }
  };

  // handlePullPage enqueues a pull_remote_batch job for every source
  // ID on the current page. The batch runs in the background; the
  // alert surfaces the job id so the user can poll /tasks.
  const handlePullPage = async () => {
    const s = slug();
    if (!s) return;
    const pageSources = sources()?.sources || [];
    if (pageSources.length === 0) return;
    const ids = pageSources.map((src) => src.id);
    setBusyPullPage(true);
    setAlert(null);
    try {
      const res = await api.pullRemoteBatch(s, ids);
      setAlert({
        variant: "success",
        message: `Pull Page enqueued — ${res.remote_source_count} sources (job: ${res.job_id})`,
      });
    } catch (err) {
      setAlert({ variant: "error", message: err.message });
    } finally {
      setBusyPullPage(false);
    }
  };

  // handlePullAllResults paginates through every source matching the
  // current search query, collects their IDs, and enqueues a single
  // pull_remote_batch job. The registry's ListSources is paginated
  // (max 200/page); we walk it with a increasing offset until we've
  // collected every ID or hit the 500-source batch cap (the backend
  // rejects larger batches with 400). The UI shows a "Collecting…"
  // state while paginating.
  const handlePullAllResults = async () => {
    const s = slug();
    if (!s) return;
    setBusyPullAllResults(true);
    setAlert(null);
    try {
      const ids = [];
      let pageOffset = 0;
      const pageSize = 200; // registry max per page
      // Cap at 500 to match the backend's maxBatch; a larger result
      // set requires the user to narrow the search.
      const cap = 500;
      while (ids.length < cap) {
        const page = await api.listRemoteSources(s, {
          q: search(),
          limit: pageSize,
          offset: pageOffset,
        });
        if (!page || !page.sources || page.sources.length === 0) break;
        for (const src of page.sources) {
          if (ids.length >= cap) break;
          ids.push(src.id);
        }
        if (page.sources.length < pageSize) break; // last page
        pageOffset += pageSize;
      }
      if (ids.length === 0) {
        setAlert({ variant: "error", message: "No sources found to pull." });
        return;
      }
      if (ids.length >= cap) {
        setAlert({
          variant: "warning",
          message: `Capped at ${cap} sources — narrow your search to pull more.`,
        });
      }
      const res = await api.pullRemoteBatch(s, ids);
      setAlert({
        variant: "success",
        message: `Pull All Results enqueued — ${res.remote_source_count} sources (job: ${res.job_id})`,
      });
    } catch (err) {
      setAlert({ variant: "error", message: err.message });
    } finally {
      setBusyPullAllResults(false);
    }
  };

  return (
    <Layout>
      <Show
        when={canRead()}
        fallback={
          <EmptyState
            title="You do not have permission to view remote sources."
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

          <RemoteContent
            hasRepo={() => !!repo.currentRepo()}
            sources={sources}
            loading={() => sources.loading}
            search={search}
            onSearch={onSearch}
            offset={offset}
            onOffsetChange={setOffset}
            total={() => sources()?.total || 0}
            limit={PAGE_SIZE}
            pullingID={pullingID}
            onPull={handlePull}
            slug={slug}
            selectedSource={selectedSource}
            onOpenDetail={setSelectedSource}
            onCloseDetail={() => setSelectedSource(null)}
            canManage={canManage}
            onPushAll={handlePushAll}
            onPullAll={handlePullAll}
            busyPushAll={busyPushAll}
            busyPullAll={busyPullAll}
            onPullPage={handlePullPage}
            onPullAllResults={handlePullAllResults}
            busyPullPage={busyPullPage}
            busyPullAllResults={busyPullAllResults}
          />
        </div>
      </Show>
    </Layout>
  );
}
