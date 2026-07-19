import { createMemo, createResource, createSignal, Show } from "solid-js";
import Layout from "../../components/Layout";
import Tabs from "../../components/Tabs";
import { api } from "../../services/api";
import { useRBAC } from "../../store/rbac";
import AIProvidersTab from "./AIProvidersTab";
import { DEFAULT_TAB, PROVIDER_TABS } from "./constants";
import DecompositionProvidersTab from "./DecompositionProvidersTab";
import EmbeddingProvidersTab from "./EmbeddingProvidersTab";
import FetchProvidersTab from "./FetchProvidersTab";
import ProvidersGate from "./ProvidersGate";
import SearchProvidersTab from "./SearchProvidersTab";

export default function Providers() {
  const rbac = useRBAC();
  const [tab, setTab] = createSignal(DEFAULT_TAB);
  const canViewProviders = createMemo(() => rbac.hasPermission("source_provider", "read"));
  const canViewAI = createMemo(() => rbac.hasPermission("ai_provider", "read"));
  const canViewDecomposition = createMemo(() => rbac.hasPermission("decomposition", "read"));

  const [providers] = createResource(canViewProviders, (can) =>
    can ? api.listProviders().catch(() => ({ search: [], resolution: [] })) : null,
  );

  const [aiProviders] = createResource(canViewAI, (can) =>
    can ? api.listAIProviders().catch(() => ({ providers: [] })) : null,
  );

  const [embeddingProviders] = createResource(canViewAI, (can) =>
    can ? api.listEmbeddingProviders().catch(() => ({ active: null, providers: [] })) : null,
  );

  const [decompositionProviders] = createResource(canViewDecomposition, (can) =>
    can
      ? api.listDecompositionProviders().catch(() => ({ chunking: [], fact_extraction: [] }))
      : null,
  );

  const aiList = () => (aiProviders() && aiProviders().providers) || [];

  return (
    <Layout>
      <Tabs tabs={PROVIDER_TABS} active={tab()} onChange={setTab} />
      <Show when={tab() === "search"}>
        <ProvidersGate can={canViewProviders} loaded={providers}>
          <SearchProvidersTab providers={providers} />
        </ProvidersGate>
      </Show>
      <Show when={tab() === "fetch"}>
        <ProvidersGate can={canViewProviders} loaded={providers}>
          <FetchProvidersTab providers={providers} />
        </ProvidersGate>
      </Show>
      <Show when={tab() === "ai"}>
        <ProvidersGate can={canViewAI} loaded={aiProviders} permission="AI providers">
          <AIProvidersTab aiProviders={aiList} />
        </ProvidersGate>
      </Show>
      <Show when={tab() === "embedding"}>
        <ProvidersGate can={canViewAI} loaded={embeddingProviders} permission="embedding providers">
          <EmbeddingProvidersTab data={embeddingProviders} />
        </ProvidersGate>
      </Show>
      <Show when={tab() === "decomposition"}>
        <ProvidersGate
          can={canViewDecomposition}
          loaded={decompositionProviders}
          permission="decomposition providers"
        >
          <DecompositionProvidersTab providers={decompositionProviders} />
        </ProvidersGate>
      </Show>
    </Layout>
  );
}
