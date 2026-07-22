import { createMemo, createSignal, Show } from "solid-js";
import EmptyState from "../../components/EmptyState";
import Layout from "../../components/Layout";
import { useRBAC } from "../../store/rbac";
import { useRepository } from "../../store/repository";
import RepoAIUsageContent from "./RepoAIUsageContent";

// RepoAIUsage is the route entry for /:slug/ai-usage. It gates on
// the ai_usage.read permission and delegates the dashboard
// composition to RepoAIUsageContent. The filter signals (from/to)
// + alert live in this entry so RepoAIUsageContent stays a thin
// view; the bucket + tab state are local to RepoAIUsageContent.
// Stays under the 150-line budget per the Page folder convention.
//
// The backend forces the repository_id filter from the URL
// context (RequireRepoPermission), so the from/to filters are the
// only client-supplied ones — repository_id is not sent.
export default function RepoAIUsage() {
  const rbac = useRBAC();
  const repo = useRepository();
  const allowed = createMemo(() => rbac.hasPermission("ai_usage", "read"));

  const [from, setFrom] = createSignal("");
  const [to, setTo] = createSignal("");
  const [alert, setAlert] = createSignal(null);

  return (
    <Layout>
      <Show
        when={allowed()}
        fallback={
          <EmptyState
            title="You don't have permission to view this repository's AI usage."
            description="Ask a sysadmin or repoadmin to grant the ai_usage.read permission."
          />
        }
      >
        <div class="space-y-6">
          <div>
            <h1 class="text-2xl font-bold text-gray-900 dark:text-white">AI Usage</h1>
            <p class="text-sm text-gray-500 dark:text-gray-400 mt-1">
              Token consumption and estimated cost over time for{" "}
              <span class="font-mono">{repo.currentRepo()?.name ?? ""}</span>.
            </p>
          </div>
          <RepoAIUsageContent
            slug={repo.currentRepo()?.slug ?? ""}
            from={from()}
            to={to()}
            alert={alert}
            onFromChange={setFrom}
            onToChange={setTo}
            onDismissAlert={() => setAlert(null)}
          />
        </div>
      </Show>
    </Layout>
  );
}
