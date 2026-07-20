import { useParams } from "@solidjs/router";
import { createMemo, createResource, createSignal, Show } from "solid-js";
import Alert from "../../components/Alert";
import EmptyState from "../../components/EmptyState";
import Layout from "../../components/Layout";
import Loading from "../../components/Loading";
import { api } from "../../services/api";
import { useRBAC } from "../../store/rbac";
import FactDetailContent from "./FactDetailContent";

// Route entry for /:slug/facts/:factID. The page owns the
// createResource that fetches {fact, sources, source_count} and
// hands them to FactDetailContent for rendering. The slug and
// factID come from URL params so the page is shareable.
export default function FactDetail() {
  const params = useParams();
  const rbac = useRBAC();
  const canRead = createMemo(() => rbac.hasPermission("fact", "read"));
  const [refreshKey, setRefreshKey] = createSignal(0);

  const [data, { refetch }] = createResource(
    () => ({ slug: params.slug, factID: params.factID, key: refreshKey() }),
    async ({ slug, factID }) => {
      if (!slug || !factID) return null;
      const res = await api.getFact(slug, factID);
      return {
        fact: res.fact,
        sources: res.sources || [],
        sourceCount: res.source_count || 0,
        concepts: res.concepts || [],
        conceptCount: res.concept_count || 0,
      };
    },
  );

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
        <Show when={!data.loading} fallback={<Loading message="Loading fact..." />}>
          <Show
            when={!data.error}
            fallback={
              <Alert
                variant="error"
                message={(data.error && data.error.message) || "Failed to load fact."}
              />
            }
          >
            <Show
              when={data()?.fact}
              fallback={
                <EmptyState
                  title="Fact not found."
                  description="The fact may have been deleted or the id is wrong."
                />
              }
            >
              <FactDetailContent
                fact={() => data().fact}
                sources={() => data().sources}
                sourceCount={() => data().sourceCount}
                concepts={() => data().concepts}
                slug={params.slug}
                onRefresh={refetch}
              />
            </Show>
          </Show>
        </Show>
      </Show>
    </Layout>
  );
}
