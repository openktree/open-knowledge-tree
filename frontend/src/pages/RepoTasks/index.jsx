import { createMemo, createSignal, Show } from "solid-js";
import EmptyState from "../../components/EmptyState";
import Layout from "../../components/Layout";
import { useRBAC } from "../../store/rbac";
import { useRepository } from "../../store/repository";
import RepoTasksContent from "./RepoTasksContent";

// RepoTasks is the route entry for /:slug/tasks. It gates on the
// task.read permission and delegates the dashboard composition to
// RepoTasksContent. The filter signals (state/kind/queue) + alert
// live inside useTasks (via RepoTasksContent) so the hook's
// createEffect re-fetches on filter change — same shape as the
// system Tasks page. Stays under the 150-line budget per the Page
// folder convention; siblings are folder-private.
//
// The backend enforces the repo scope (RequireRepoPermission reads
// the repo UUID from the URL), so RepoTasksContent passes the slug
// in the path — listRepoTasks / getRepoTaskStats use the slug and
// the server forces the repo_id metadata filter.
export default function RepoTasks() {
  const rbac = useRBAC();
  const repo = useRepository();
  const allowed = createMemo(() => rbac.hasPermission("task", "read"));

  return (
    <Layout>
      <Show
        when={allowed()}
        fallback={
          <EmptyState
            title="You don't have permission to view this repository's tasks."
            description="Ask a repoadmin or sysadmin to grant the task.read permission."
          />
        }
      >
        <div class="space-y-6">
          <div>
            <h1 class="text-2xl font-bold text-gray-900 dark:text-white">Tasks</h1>
            <p class="text-sm text-gray-500 dark:text-gray-400 mt-1">
              Background job queue for{" "}
              <span class="font-mono">{repo.currentRepo()?.name ?? ""}</span>.
            </p>
          </div>
          <RepoTasksContent slug={repo.currentRepo()?.slug ?? ""} repo={repo.currentRepo()} />
        </div>
      </Show>
    </Layout>
  );
}
