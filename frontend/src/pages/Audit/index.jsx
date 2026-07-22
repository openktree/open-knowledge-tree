import { createMemo, createSignal, Show } from "solid-js";
import EmptyState from "../../components/EmptyState";
import Layout from "../../components/Layout";
import { useRBAC } from "../../store/rbac";
import AuditContent from "./AuditContent";

// Audit is the route entry for the /audit page. It owns the
// page-level filter state (from / to / action / actorUserID) +
// alert state, gates on the audit.read permission, and delegates
// the dashboard composition to AuditContent. Stays under the
// 150-line budget per the Page folder convention; siblings are
// folder-private.
//
// The system view shows every audit row (sysadmin only). The
// per-repo view lives as a tab inside the repository view, not
// here; it calls api.listRepoAudit directly with the repo slug.
export default function Audit() {
  const rbac = useRBAC();
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
            title="You don't have permission to view the audit log."
            description="Ask a sysadmin or repoadmin to grant the audit.read permission."
          />
        }
      >
        <div class="space-y-6">
          <div>
            <h1 class="text-2xl font-bold text-gray-900 dark:text-white">Audit Log</h1>
            <p class="text-sm text-gray-500 dark:text-gray-400 mt-1">
              RBAC changes, admin actions, and source ingestion starts across the whole system.
            </p>
          </div>
          <AuditContent
            from={from()}
            to={to()}
            action={action()}
            actorUserID={actorUserID()}
            alert={alert}
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
