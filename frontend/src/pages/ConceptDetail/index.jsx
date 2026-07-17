import { Show, createResource, createMemo, createSignal } from "solid-js";
import { useParams } from "@solidjs/router";
import { api } from "../../services/api";
import Layout from "../../components/Layout";
import Loading from "../../components/Loading";
import EmptyState from "../../components/EmptyState";
import Alert from "../../components/Alert";
import ConceptHeader from "./ConceptHeader";
import ContextPanel from "./ContextPanel";
import ConceptRelations from "./ConceptRelations";
import DefinitionPanel from "./DefinitionPanel";

// ConceptDetail is the detail page for a single concept group,
// addressed by any concept_id in the group (the backend resolves the
// id to its canonical_name group). The page shows the group header
// (canonical name, total fact count, and a row of context tabs)
// plus the selected context's panel (description, metadata,
// aliases, and the facts linked to that context's concept_id —
// facts are compartmentalized per context).
//
// The active context defaults to the highest-fact_count context
// (the backend orders contexts by fact_count DESC) and switches
// when the user clicks a tab. The facts fetch is keyed on the
// active context's concept_id, so switching tabs re-fetches the
// per-context facts.
export default function ConceptDetail() {
  const params = useParams();
  const slug = () => params.slug;
  const conceptID = () => params.conceptID;

  const [groupData, { refetch }] = createResource(
    () => ({ slug: slug(), conceptID: conceptID() }),
    async ({ slug, conceptID }) => {
      if (!slug || !conceptID) return null;
      try {
        return await api.getConcept(slug, conceptID);
      } catch (err) {
        return { error: err };
      }
    }
  );

  const [activeIndex, setActiveIndex] = createSignal(0);

  const group = () => (groupData() && !groupData()?.error ? groupData() : null);
  const contexts = () => group()?.contexts || [];
  const activeContext = createMemo(() => contexts()[activeIndex()] || null);

  return (
    <Layout maxWidth="max-w-[1400px]">
      <Show when={!groupData.loading} fallback={<Loading message="Loading concept..." />}>
        <Show
          when={!groupData()?.error}
          fallback={<Alert variant="error" message={groupData()?.error?.message || "Failed to load concept."} />}
        >
          <Show
            when={group()}
            fallback={<EmptyState title="Concept not found." description="The concept may have been deleted or the concept id is wrong." />}
          >
            <div class="grid grid-cols-1 xl:grid-cols-3 gap-6 items-start">
              <div class="xl:col-span-2 space-y-6">
              <ConceptHeader
                group={group()}
                activeIndex={activeIndex()}
                activeContext={activeContext()}
                onSelectContext={setActiveIndex}
                onRefresh={refetch}
              />
                <Show when={contexts().length > 0} fallback={<EmptyState title="No contexts for this concept." />}>
                  <DefinitionPanel slug={slug()} conceptID={contexts()[0]?.concept_id} />
                  <ContextPanel slug={slug()} context={activeContext()} />
                </Show>
              </div>
              <div class="xl:sticky xl:top-8">
                <ConceptRelations slug={slug()} conceptID={conceptID()} />
              </div>
            </div>
          </Show>
        </Show>
      </Show>
    </Layout>
  );
}