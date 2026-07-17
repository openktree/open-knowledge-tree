import { createMemo, createResource, Show, createSignal } from "solid-js";
import { api } from "../../services/api";
import { useRBAC } from "../../store/rbac";
import { useRepository } from "../../store/repository";
import Layout from "../../components/Layout";
import EmptyState from "../../components/EmptyState";
import Loading from "../../components/Loading";
import ConceptsContent from "./ConceptsContent";
import { PAGE_SIZE } from "./constants";

// Concepts is the repo-level concepts list page. Mirrors the Facts
// page: a createResource keyed on the active repo slug + offset
// calling api.listRepoConcepts, RBAC-gated on concept:read.
// Concepts are produced automatically by the extract_concepts
// worker chained after dedup, so this page is read-only in Phase 1.
export default function Concepts() {
  const rbac = useRBAC();
  const repo = useRepository();
  const canRead = createMemo(() => rbac.hasPermission("concept", "read"));
  const [search, setSearch] = createSignal("");
  const [offset, setOffset] = createSignal(0);

  const [concepts, { refetch }] = createResource(
    () => ({ slug: repo.currentRepo()?.slug || "", q: search(), offset: offset() }),
    async ({ slug, q, offset }) => {
      if (!slug) return null;
      try {
        return await api.listRepoConcepts(slug, { q, limit: PAGE_SIZE, offset });
      } catch {
        return null;
      }
    }
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
            title="You do not have permission to view concepts."
            description="Ask a repository admin to grant you the source:read permission."
          />
        }
      >
        <Show when={!concepts.loading} fallback={<Loading message="Loading concepts..." />}>
          <ConceptsContent
            concepts={concepts}
            slug={() => repo.currentRepo()?.slug || ""}
            hasRepo={() => !!repo.currentRepo()}
            onRefresh={refetch}
            search={search}
            onSearch={onSearch}
            offset={offset}
            onOffsetChange={setOffset}
            total={() => concepts()?.total || 0}
            limit={PAGE_SIZE}
          />
        </Show>
      </Show>
    </Layout>
  );
}