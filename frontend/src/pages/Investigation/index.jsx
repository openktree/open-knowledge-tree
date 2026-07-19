import { useParams } from "@solidjs/router";
import { createResource, createSignal, Show } from "solid-js";
import Alert from "../../components/Alert";
import EmptyState from "../../components/EmptyState";
import Layout from "../../components/Layout";
import Loading from "../../components/Loading";
import { api } from "../../services/api";
import { ACTIVE_PHASE_IDS } from "./constants";
import InvestigationConceptsPhase from "./InvestigationConceptsPhase";
import InvestigationFactsPhase from "./InvestigationFactsPhase";
import InvestigationSourcesPhase from "./InvestigationSourcesPhase";
import InvestigationStepper from "./InvestigationStepper";

export default function Investigation() {
  const params = useParams();
  const slug = () => params.slug;
  const invID = () => params.invID;
  const phase = () => (ACTIVE_PHASE_IDS.includes(params.phase) ? params.phase : "sources");
  const [alert, setAlert] = createSignal(null);

  const [inv, { refetch: refetchInv }] = createResource(
    () => [slug(), invID()],
    async ([s, id]) => {
      if (!s || !id) return null;
      try {
        return await api.getInvestigation(s, id);
      } catch (err) {
        setAlert({ variant: "error", message: err.message });
        return null;
      }
    },
  );

  return (
    <Layout>
      <div class="space-y-6">
        <Alert
          variant={alert()?.variant}
          message={alert()?.message}
          onDismiss={() => setAlert(null)}
        />
        <Show when={!inv.loading} fallback={<Loading message="Loading investigation..." />}>
          <Show
            when={inv()}
            fallback={
              <EmptyState title="Investigation not found" description="It may have been deleted." />
            }
          >
            <header class="space-y-3">
              <div>
                <h1 class="text-2xl font-bold dark:text-white">{inv().title}</h1>
                <Show when={inv().topic}>
                  <p class="text-sm text-gray-600 dark:text-gray-400 mt-1">{inv().topic}</p>
                </Show>
              </div>
              <InvestigationStepper slug={slug()} invID={invID()} />
            </header>
            <Show when={phase() === "sources"}>
              <InvestigationSourcesPhase
                slug={slug()}
                invID={invID()}
                inv={inv}
                refetchInv={refetchInv}
                onAlert={setAlert}
              />
            </Show>
            <Show when={phase() === "facts"}>
              <InvestigationFactsPhase slug={slug()} invID={invID()} onAlert={setAlert} />
            </Show>
            <Show when={phase() === "concepts"}>
              <InvestigationConceptsPhase slug={slug()} invID={invID()} onAlert={setAlert} />
            </Show>
          </Show>
        </Show>
      </div>
    </Layout>
  );
}
