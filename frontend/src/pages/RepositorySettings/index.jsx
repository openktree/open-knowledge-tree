// @okt-page-allow-large: thin orchestrator composing 9 sibling panels; splitting further would contrive a layer

import { useParams } from "@solidjs/router";
import { createResource, createSignal, Show } from "solid-js";
import Alert from "../../components/Alert";
import Card from "../../components/Card";
import Layout from "../../components/Layout";
import Loading from "../../components/Loading";
import { api } from "../../services/api";
import ContentTypesPanel from "./ContentTypesPanel";
import ContextMappingsPanel from "./ContextMappingsPanel";
import ContextsPanel from "./ContextsPanel";
import ContributorPanel from "./ContributorPanel";
import ModelsPanel from "./ModelsPanel";
import PromptsetPanel from "./PromptsetPanel";
import ProvidersPanel from "./ProvidersPanel";
import RegistryPanel from "./RegistryPanel";
import RegistrySyncPanel from "./RegistrySyncPanel";

// RepositorySettings is the repo-admin settings surface (distinct
// from the global /repositories page). Gated by repository:manage.
export default function RepositorySettings() {
  const params = useParams();
  const repoID = () => params.repoID;
  const [alert, setAlert] = createSignal(null);
  const [settings, { refetch }] = createResource(repoID, (id) =>
    id
      ? api.getRepositorySettings(id).catch((e) => {
          setAlert({ variant: "error", message: e.message });
          return null;
        })
      : null,
  );

  const data = () => settings() || { providers: [], contexts: [] };

  return (
    <Layout>
      <div class="space-y-6">
        <Alert
          variant={alert()?.variant}
          message={alert()?.message}
          onDismiss={() => setAlert(null)}
        />
        <Show when={!settings.loading} fallback={<Loading message="Loading settings…" />}>
          <Card>
            <h2 class="text-lg font-semibold dark:text-white">Repository Settings</h2>
            <p class="text-sm text-gray-500 dark:text-gray-400 mt-1">
              Repository ID: <span class="font-mono">{repoID()}</span>
            </p>
          </Card>
          <ProvidersPanel
            repoID={repoID}
            providers={() => data().providers}
            onChanged={refetch}
            onAlert={setAlert}
          />
          <ModelsPanel
            repoID={repoID}
            taskModels={() => data().task_models}
            catalog={() => data().model_catalog}
            onChanged={refetch}
            onAlert={setAlert}
          />
          <PromptsetPanel repoID={repoID} onAlert={setAlert} />
          <RegistryPanel
            repoID={repoID}
            registryID={() => data().registry_id}
            registryEnabled={() => data().registry_enabled}
            registryOptions={() => data().registry_options}
            registryConfigured={() => data().registry_configured}
            autoContribute={() => data().auto_contribute}
            allowedModels={() => data().allowed_models}
            allowedModelsDefault={() => data().allowed_models_default}
            catalog={() => data().model_catalog}
            onChanged={refetch}
            onAlert={setAlert}
          />
          <RegistrySyncPanel
            repoID={repoID}
            registryConfigured={() => data().registry_configured}
            registryEnabled={() => data().registry_enabled}
            pushLevel={() => data().registry_push_level}
            pullLevel={() => data().registry_pull_level}
            onAlert={setAlert}
            onChanged={refetch}
          />
          <ContentTypesPanel
            repoID={repoID}
            allowedContentTypes={() => data().allowed_content_types}
            onChanged={refetch}
            onAlert={setAlert}
          />
          <ContributorPanel
            repoID={repoID}
            displayName={() => data().contributor_display_name}
            anonymous={() => data().contributor_anonymous}
            onChanged={refetch}
            onAlert={setAlert}
          />
          <Card>
            <ContextsPanel
              repoID={repoID}
              contexts={() => data().contexts}
              onChanged={refetch}
              onAlert={setAlert}
            />
          </Card>
          <Card>
            <ContextMappingsPanel
              repoID={repoID}
              mappings={() => data().context_mappings}
              unmappedLocal={() => data().unmapped_local}
              registryContexts={() => data().registry_contexts}
              unmappedPolicy={() => data().unmapped_policy}
              catchAllContext={() => data().catch_all_context}
              contexts={() => data().contexts}
              onChanged={refetch}
              onAlert={setAlert}
            />
          </Card>
        </Show>
      </div>
    </Layout>
  );
}
