// @okt-page-allow-large: folder page; checker miscounts default export as internal subcomponent
import { createMemo, createResource, createSignal, Show, For } from "solid-js";
import { useParams, A } from "@solidjs/router";
import { api } from "../../services/api";
import { useRBAC } from "../../store/rbac";
import Layout from "../../components/Layout";
import Loading from "../../components/Loading";
import EmptyState from "../../components/EmptyState";
import Alert from "../../components/Alert";
import CitedFactModal from "../../components/CitedFactModal";
import ReportDetailContent from "./ReportDetailContent";

export default function ReportDetail() {
  const params = useParams();
  const rbac = useRBAC();
  const canRead = createMemo(() => rbac.hasPermission("report", "read"));
  const canUpdate = createMemo(() => rbac.hasPermission("report", "update"));
  const [refreshKey, setRefreshKey] = createSignal(0);
  const [alert, setAlert] = createSignal(null);
  const [annotating, setAnnotating] = createSignal(false);

  const [data, { refetch }] = createResource(
    () => ({ slug: params.slug, reportID: params.reportID, key: refreshKey() }),
    async ({ slug, reportID }) => {
      if (!slug || !reportID) return null;
      const res = await api.getReport(slug, reportID);
      return {
        report: res.report || {},
        annotations: res.annotations || [],
      };
    }
  );

  // Fetch the full report list for this repo so we can show the
  // parent report title and the sub-reports (children) of the
  // current report without extra round-trips per relationship.
  const [allReports] = createResource(
    () => [params.slug, refreshKey()],
    async ([slug]) => {
      if (!slug) return [];
      try {
        const res = await api.listReports(slug, { limit: 200 });
        return res.data || [];
      } catch {
        return [];
      }
    }
  );

  const parentReport = createMemo(() => {
    const pid = data()?.report?.parent_id;
    if (!pid) return null;
    return allReports()?.find((r) => r.id === pid) || { id: pid, title: pid };
  });

  const children = createMemo(() => {
    const id = data()?.report?.id;
    if (!id) return [];
    return (allReports() || []).filter((r) => r.parent_id === id);
  });

  const annotations = () => data()?.annotations || [];

  const highlightIndices = createMemo(() => {
    const anns = annotations();
    if (!anns.length) return null;
    const set = new Set();
    for (const a of anns) set.add(a.sentence_index);
    return set;
  });

  const factCounts = createMemo(() => {
    const anns = annotations();
    if (!anns.length) return null;
    const map = new Map();
    for (const a of anns) {
      map.set(a.sentence_index, (map.get(a.sentence_index) || 0) + 1);
    }
    return map;
  });

  const factsBySentence = createMemo(() => {
    const anns = annotations();
    if (!anns.length) return new Map();
    const map = new Map();
    for (const a of anns) {
      const arr = map.get(a.sentence_index);
      if (arr) arr.push(a);
      else map.set(a.sentence_index, [a]);
    }
    return map;
  });

  const [activeSentence, setActiveSentence] = createSignal(null);
  const activeFacts = () =>
    activeSentence() == null ? [] : factsBySentence().get(activeSentence()) || [];
  const closeModal = () => setActiveSentence(null);

  const handleAnnotate = async () => {
    setAnnotating(true);
    setAlert(null);
    try {
      await api.annotateReport(params.slug, params.reportID);
      setAlert({ variant: "success", message: "Re-annotation queued." });
      setTimeout(() => setRefreshKey((k) => k + 1), 1000);
    } catch (err) {
      setAlert({ variant: "error", message: err.message });
    } finally {
      setAnnotating(false);
    }
  };

  return (
    <Layout>
      <Show when={canRead()} fallback={<EmptyState title="Permission required" description="You do not have permission to read reports." />}>
        <Show when={!data.loading} fallback={<Loading message="Loading report..." />}>
          <Show when={!data.error} fallback={<Alert variant="error" message={data.error?.message || "Failed to load report"} />}>
            <Show when={data()?.report} fallback={<EmptyState title="Report not found" description="This report may have been deleted." />}>
              <div class="space-y-4">
                <Show when={alert()}>
                  <Alert variant={alert()?.variant} message={alert()?.message} onDismiss={() => setAlert(null)} />
                </Show>
                <ReportDetailContent
                  report={() => data().report}
                  annotations={annotations}
                  slug={params.slug}
                  canUpdate={canUpdate}
                  annotating={annotating}
                  onRefresh={refetch}
                  onAnnotate={handleAnnotate}
                  onSentenceClick={setActiveSentence}
                  onAlert={setAlert}
                  parentReport={parentReport}
                  children={children}
                />
              </div>
            </Show>
          </Show>
        </Show>
      </Show>
      <CitedFactModal
        open={activeSentence() != null}
        onClose={closeModal}
        sentenceIndex={activeSentence()}
        facts={activeFacts()}
        slug={params.slug}
      />
    </Layout>
  );
}