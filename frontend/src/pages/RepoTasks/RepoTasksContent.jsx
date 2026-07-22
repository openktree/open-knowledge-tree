import { createMemo, createSignal, Show } from "solid-js";
import Alert from "../../components/Alert";
import Button from "../../components/Button";
import Card from "../../components/Card";
import { useRBAC } from "../../store/rbac";
import JobDetail from "../Tasks/JobDetail";
import TasksFilters from "../Tasks/TasksFilters";
import TasksStats from "../Tasks/TasksStats";
import TasksTable from "../Tasks/TasksTable";
import { useTasks } from "../Tasks/useTasks";

// RepoTasksContent composes the per-repo tasks dashboard. The
// parent (index.jsx) gates the permission and renders the page
// chrome; this component owns the selected-job signal and the
// useTasks hook instance (scoped to the repo via the slug prop).
//
// canRescue is false — rescue is system-only (POST /admin/tasks/rescue
// is gated on task.manage at system scope).
//
// canReextract is gated on repositories.*.manage (repoadmin+sysadmin)
// and the repo object is passed so the danger box skips the repo
// picker (single repo).
export default function RepoTasksContent(props) {
  const t = useTasks(props.slug);
  const rbac = useRBAC();
  const [selectedJob, setSelectedJob] = createSignal(null);
  const canReextract = createMemo(() => rbac.hasPermission("repositories", "manage"));

  const reload = () => {
    setSelectedJob(null);
    t.reload();
  };

  return (
    <div class="space-y-6">
      <Alert
        variant={t.alert()?.variant}
        message={t.alert()?.message}
        onDismiss={() => t.setAlert(null)}
      />
      <TasksStats
        stats={t.stats()}
        loading={t.statsLoading()}
        onRefresh={t.reloadStats}
        refreshing={t.statsLoading()}
        canRescue={false}
        rescuing={t.rescuing()}
        onRescue={t.rescueStuckJobs}
        canReextract={canReextract()}
        reextracting={t.reextracting()}
        onReextract={t.reextractConcepts}
        repositories={props.repo ? [props.repo] : []}
        currentRepo={props.repo}
      />
      <Show
        when={!selectedJob()}
        fallback={<JobDetail job={selectedJob()} onBack={() => setSelectedJob(null)} />}
      >
        <TasksFilters
          state={t.state()}
          kind={t.kind()}
          queue={t.queue()}
          loading={t.loading()}
          onStateChange={t.setState}
          onKindChange={t.setKind}
          onQueueChange={t.setQueue}
          onRefresh={reload}
        />
        <Show
          when={t.jobs().length > 0}
          fallback={
            <Card>
              <p class="text-gray-400 dark:text-gray-500 text-sm text-center py-4">
                {t.loading()
                  ? "Loading..."
                  : "No tasks found for this repository. Tasks will appear here once jobs are enqueued."}
              </p>
            </Card>
          }
        >
          <TasksTable jobs={t.jobs()} onSelectJob={setSelectedJob} />
          <Show when={t.hasMore()}>
            <div class="flex justify-center">
              <Button
                variant="secondary"
                onClick={t.loadMore}
                loading={t.loadingMore()}
                loadingText="Loading..."
              >
                Load more
              </Button>
            </div>
          </Show>
        </Show>
      </Show>
    </div>
  );
}
