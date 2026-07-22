import { createResource, createSignal, Show } from "solid-js";
import Alert from "../../components/Alert";
import AuditFilterBar from "../../components/Audit/AuditFilterBar";
import AuditTable from "../../components/Audit/AuditTable";
import EmptyState from "../../components/EmptyState";
import Loading from "../../components/Loading";
import Pagination from "../../components/Pagination";
import { api } from "../../services/api";

// RepoAuditContent composes the per-repo audit dashboard. The
// parent (index.jsx) owns the filter signals; this component
// owns the pagination state and fetches the audit list via
// api.listRepoAudit (which puts the slug in the URL path so the
// backend's RequireRepoPermission middleware enforces scope).
// The fetch refetches whenever the slug, filters, or pagination
// change.
//
// The AuditTable is rendered with showRepo=false because every
// row is already scoped to this repo (the column would be
// redundant). The actor filter is kept (a repoadmin may want to
// see "what did this one user do in my repo").
export default function RepoAuditContent(props) {
  const [offset, setOffset] = createSignal(0);
  const limit = 100;

  // setAlert forwards to the parent's alert signal so the alert
  // renders in the page chrome above the filter bar. Defined
  // before the createResource so the fetch's catch closure can
  // call it without a forward-reference.
  const setAlert = (a) => props.onAlertChange?.(a);

  const params = () => ({
    slug: props.slug,
    from: props.from || undefined,
    to: props.to || undefined,
    action: props.action || undefined,
    actor_user_id: props.actorUserID || undefined,
    limit,
    offset: offset(),
  });

  const [data, { refetch }] = createResource(params, async (p) => {
    if (!p.slug) return null;
    try {
      return await api.listRepoAudit(p.slug, {
        from: p.from,
        to: p.to,
        action: p.action,
        actor_user_id: p.actor_user_id,
        limit: p.limit,
        offset: p.offset,
      });
    } catch (err) {
      setAlert({ variant: "error", message: err.message });
      return null;
    }
  });

  const events = () => data()?.events ?? [];
  const total = () => data()?.total ?? 0;
  const actions = () => data()?.actions ?? [];

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
        onRefresh={() => {
          setOffset(0);
          refetch();
        }}
      />

      <Show when={!data.loading} fallback={<Loading message="Loading audit log…" />}>
        <AuditTable events={events()} showRepo={false} />
      </Show>

      <Show when={total() > limit}>
        <Pagination total={total()} limit={limit} offset={offset()} onOffsetChange={setOffset} />
      </Show>

      <Show when={!data.loading && total() === 0 && !props.from && !props.to && !props.action}>
        <EmptyState
          title="No audit events for this repository yet."
          description="Audit events appear here after the first settings change, source ingestion, or role assignment in this repo."
        />
      </Show>
    </div>
  );
}
