import { createMemo, createResource, createSignal, Show } from "solid-js";
import EmptyState from "../../components/EmptyState";
import Layout from "../../components/Layout";
import Loading from "../../components/Loading";
import { api } from "../../services/api";
import { useRBAC } from "../../store/rbac";
import { useRepository } from "../../store/repository";
import { SORT_OPTIONS, STATUS_OPTIONS } from "./constants";
import FactsContent from "./FactsContent";

const PAGE_SIZE = 100;

export default function Facts() {
  const rbac = useRBAC();
  const repo = useRepository();
  const canRead = createMemo(() => rbac.hasPermission("fact", "read"));
  const [statusFilter, setStatusFilter] = createSignal("stable");
  const [sort, setSort] = createSignal("created_at");
  const [search, setSearch] = createSignal("");
  const [offset, setOffset] = createSignal(0);

  const [facts, { refetch }] = createResource(
    () => ({
      slug: repo.currentRepo()?.slug || "",
      status: statusFilter(),
      sort: sort(),
      q: search(),
      offset: offset(),
    }),
    async ({ slug, status, sort, q, offset }) => {
      if (!slug) return null;
      try {
        return await api.listRepoFacts(slug, status, sort, { q, limit: PAGE_SIZE, offset });
      } catch {
        return null;
      }
    },
  );

  const onSearch = (q) => {
    setSearch(q);
    setOffset(0);
  };

  return (
    <Layout>
      <Show
        when={canRead()}
        fallback={
          <EmptyState
            title="You do not have permission to view facts."
            description="Ask a repository admin to grant you the source:read permission."
          />
        }
      >
        <Show when={!facts.loading} fallback={<Loading message="Loading facts..." />}>
          <FactsContent
            facts={facts}
            slug={() => repo.currentRepo()?.slug || ""}
            hasRepo={() => !!repo.currentRepo()}
            onRefresh={refetch}
            statusFilter={statusFilter}
            onStatusChange={(v) => {
              setStatusFilter(v);
              setOffset(0);
            }}
            statusOptions={STATUS_OPTIONS}
            sort={sort}
            onSortChange={(v) => {
              setSort(v);
              setOffset(0);
            }}
            sortOptions={SORT_OPTIONS}
            search={search}
            onSearch={onSearch}
            offset={offset}
            onOffsetChange={setOffset}
            total={() => facts()?.total || 0}
            limit={PAGE_SIZE}
          />
        </Show>
      </Show>
    </Layout>
  );
}
