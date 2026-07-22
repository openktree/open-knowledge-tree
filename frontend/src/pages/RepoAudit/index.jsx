import { createMemo, createSignal, Show } from "solid-js";
import EmptyState from "../../components/EmptyState";
import Layout from "../../components/Layout";
import { useRBAC } from "../../store/rbac";
import { useRepository } from "../../store/repository";
import RepoAuditContent from "./RepoAuditContent";

// RepoAudit is the route entry for /repositories/:slug/audit. It
// owns the page-level filter state (from / to / action /
// actorUserID) + alert state, gates on the audit.read
// permission, and delegates the dashboard composition to
// RepoAuditContent. Stays under the 150-line budget per the
// Page folder convention; siblings are folder-private.
//
// The backend enforces the repo scope (RequireRepoPermission
// reads the repo UUID from the URL), so RepoAuditContent passes
// no repository_id filter — listRepoAudit uses the slug in the
// path.
export default function RepoAudit() {
  const rbac = useRBAC();
  const repo = useRepository();
  const allowed = createMemo(() => rbac.hasPermission("audit", "read"));

  const [from, setFrom] = createSignal("");
  const [to, setTo] = createSignal("");
  const [action, setAction] = createSignal("");
  const [actorUserID, setActorUserID] = createSignal("");
  const [alert, setAlert] = createSignal(null);

  return (
    <Layout>
      <Show
        when={allowed()}
        fallback={
          <EmptyState
            title="You don't have permission to view this repository's audit log."
            description="Ask a repoadmin or sysadmin to grant the audit.read permission."
          />
        }
      >
        <div class="space-y-6">
          <div>
            <h1 class="text-2xl font-bold text-gray-900 dark:text-white">Audit Log</h1>
            <p class="text-sm text-gray-500 dark:text-gray-400 mt-1">
              RBAC changes, settings mutations, and source ingestion starts for{" "}
              <span class="font-mono">{repo.currentRepo()?.name ?? ""}</span>.
            </p>
          </div>
          <RepoAuditContent
            slug={repo.currentRepo()?.slug ?? ""}
            from={from()}
            to={to()}
            action={action()}
            actorUserID={actorUserID()}
            alert={alert}
            onAlertChange={setAlert}
            onFromChange={setFrom}
            onToChange={setTo}
            onActionChange={setAction}
            onActorUserIDChange={setActorUserID}
            onDismissAlert={() => setAlert(null)}
          />
        </div>
      </Show>
    </Layout>
  );
}
