import { createMemo, createResource, createSignal, Show } from "solid-js";
import Alert from "../../components/Alert";
import AuditFilterBar from "../../components/Audit/AuditFilterBar";
import AuditTable from "../../components/Audit/AuditTable";
import EmptyState from "../../components/EmptyState";
import Loading from "../../components/Loading";
import Pagination from "../../components/Pagination";
import { api } from "../../services/api";

// AuditContent composes the system audit dashboard. The parent
// (index.jsx) owns the filter signals; this component owns the
// pagination state and fetches the audit list + the distinct
// actions list (for the filter dropdown). The fetch refetches
// whenever the filters or pagination change.
//
// The system view passes no repository_id filter (the backend
// returns every row). Repo-scoped audit lives in the repository
// tab and calls api.listRepoAudit directly; this component is
// system-only.
export default function AuditContent(props) {
  const [offset, setOffset] = createSignal(0);
  const limit = 100;

  const params = () => ({
    from: props.from || undefined,
    to: props.to || undefined,
    action: props.action || undefined,
    actor_user_id: props.actorUserID || undefined,
    limit,
    offset: offset(),
  });

  const [data, { refetch }] = createResource(params, (p) => api.listSystemAudit(p));
  const [actionsData] = createResource(() => api.listSystemAudit({ limit: 1 }));

  const events = () => data()?.events ?? [];
  const total = () => data()?.total ?? 0;
  const actions = () => actionsData()?.actions ?? [];

  const onRefresh = () => {
    setOffset(0);
    refetch();
  };

  return (
    <div class="space-y-6">
      <Alert
        variant={props.alert()?.variant}
        message={props.alert()?.message}
        onDismiss={() => props.onDismissAlert()}
      />

      <AuditFilterBar
        from={props.from}
        to={props.to}
        action={props.action}
        actorUserID={props.actorUserID}
        actions={actions()}
        onFromChange={props.onFromChange}
        onToChange={props.onToChange}
        onActionChange={props.onActionChange}
        onActorUserIDChange={props.onActorUserIDChange}
        onRefresh={onRefresh}
      />

      <Show when={!data.loading} fallback={<Loading message="Loading audit log…" />}>
        <AuditTable events={events()} showRepo={true} />
      </Show>

      <Show when={total() > limit}>
        <Pagination total={total()} limit={limit} offset={offset()} onOffsetChange={setOffset} />
      </Show>

      <Show when={!data.loading && total() === 0 && !props.from && !props.to && !props.action}>
        <EmptyState
          title="No audit events yet."
          description="Audit events appear here after the first RBAC change, repository mutation, or source ingestion."
        />
      </Show>
    </div>
  );
}
