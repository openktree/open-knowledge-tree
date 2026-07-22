import { createSignal, Show } from "solid-js";
import Alert from "../../components/Alert";
import Button from "../../components/Button";
import Card from "../../components/Card";
import Layout from "../../components/Layout";
import { useRBAC } from "../../store/rbac";
import { useRepository } from "../../store/repository";
import JobDetail from "./JobDetail";
import TasksFilters from "./TasksFilters";
import TasksStats from "./TasksStats";
import TasksTable from "./TasksTable";
import { useTasks } from "./useTasks";

export default function Tasks() {
  const t = useTasks();
  const rbac = useRBAC();
  const repo = useRepository();
  const [selectedJob, setSelectedJob] = createSignal(null);
  const canRescue = () => rbac.hasPermission("task", "manage");
  const canReextract = () => rbac.hasPermission("repositories", "manage");

  const reload = () => {
    setSelectedJob(null);
    t.reload();
  };

  return (
    <Layout>
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
          canRescue={canRescue()}
          rescuing={t.rescuing()}
          onRescue={t.rescueStuckJobs}
          canReextract={canReextract()}
          reextracting={t.reextracting()}
          onReextract={t.reextractConcepts}
          repositories={repo.repositories()}
          currentRepo={repo.currentRepo()}
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
                    : "No tasks found. Tasks will appear here once jobs are enqueued."}
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
    </Layout>
  );
}
