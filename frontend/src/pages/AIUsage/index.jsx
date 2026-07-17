import { createMemo, createSignal, Show } from "solid-js";
import Layout from "../../components/Layout";
import EmptyState from "../../components/EmptyState";
import { useRBAC } from "../../store/rbac";
import AIUsageContent from "./AIUsageContent";

// AIUsage is the route entry for the /ai-usage page. It owns the
// page-level filter state (from/to/repoID) + alert state, gates on
// the ai_usage.read permission, and delegates the dashboard
// composition to AIUsageContent. Stays under the 150-line budget
// per the Page folder convention; siblings are folder-private.
export default function AIUsage() {
  const rbac = useRBAC();
  const allowed = createMemo(() => rbac.hasPermission("ai_usage", "read"));

  const [from, setFrom] = createSignal("");
  const [to, setTo] = createSignal("");
  const [repoID, setRepoID] = createSignal("");
  const [alert, setAlert] = createSignal(null);

  return (
    <Layout>
      <Show
        when={allowed()}
        fallback={
          <EmptyState
            title="You don't have permission to view AI usage."
            description="Ask a sysadmin to grant the ai_usage.read permission."
          />
        }
      >
        <div class="space-y-6">
          <div>
            <h1 class="text-2xl font-bold text-gray-900 dark:text-white">AI Usage</h1>
            <p class="text-sm text-gray-500 dark:text-gray-400 mt-1">
              Token consumption and estimated cost over time, per model, provider, repository, and source.
            </p>
          </div>
          <AIUsageContent
            from={from()}
            to={to()}
            repoID={repoID()}
            alert={alert}
            onFromChange={setFrom}
            onToChange={setTo}
            onRepoIDChange={setRepoID}
            onDismissAlert={() => setAlert(null)}
          />
        </div>
      </Show>
    </Layout>
  );
}