import { useParams } from "@solidjs/router";
import { createMemo, createResource, createSignal, Show } from "solid-js";
import Alert from "../../components/Alert";
import Badge from "../../components/Badge";
import Button from "../../components/Button";
import EmptyState from "../../components/EmptyState";
import Layout from "../../components/Layout";
import Loading from "../../components/Loading";
import { api } from "../../services/api";
import { useRBAC } from "../../store/rbac";
import { parseStatusCopy } from "./constants";
import FetchErrorState from "./FetchErrorState";
import ParseEmptyState from "./ParseEmptyState";
import SourceDetailContent from "./SourceDetailContent";
import SourceFacts from "./SourceFacts";
import SourceSentenceModal from "./SourceSentenceModal";

/**
 * Route body for /:slug/sources/:sourceID.
 *
 * State ownership:
 *   - The (slug, sourceID) pair comes from URL params
 *     so the page is shareable.
 *   - The source row and image list are fetched in a
 *     single createResource; the backend already returns
 *     them together as {source, images}.
 *   - Manual refetch on demand (the "Refresh" button)
 *     covers the case where the worker is still parsing;
 *     the user can come back and pull fresh state
 *     without a full page reload.
 *
 * Lives in its own file rather than the route index
 * because the file is large enough that defining the
 * default-export function here would push the route
 * entry above the page-size-policy limit. The route
 * index is a one-line re-export.
 */
export default function SourceDetailPage() {
  const params = useParams();
  const rbac = useRBAC();
  const canRead = createMemo(() => rbac.hasPermission("source", "read"));
  const canUpdate = createMemo(() => rbac.hasPermission("source", "update"));
  const [refreshKey, setRefreshKey] = createSignal(0);
  const [processAlert, setProcessAlert] = createSignal(null);
  const [processing, setProcessing] = createSignal(false);
  const [factsSearch, setFactsSearch] = createSignal("");
  const [factsOffset, setFactsOffset] = createSignal(0);
  const factsPageLimit = 100;

  const [data] = createResource(
    () => ({ slug: params.slug, sourceID: params.sourceID, key: refreshKey() }),
    async ({ slug, sourceID }) => {
      if (!slug || !sourceID) return null;
      const res = await api.getSource(slug, sourceID);
      return { source: res.source, images: res.images || [] };
    },
  );

  const [factsData, { refetch: refetchFacts }] = createResource(
    () => ({
      slug: params.slug,
      sourceID: params.sourceID,
      key: refreshKey(),
      q: factsSearch(),
      offset: factsOffset(),
    }),
    async ({ slug, sourceID, q, offset }) => {
      if (!slug || !sourceID) return null;
      try {
        return await api.listFacts(slug, sourceID, "", { q, limit: factsPageLimit, offset });
      } catch {
        return null;
      }
    },
  );

  // Sentence-level provenance for the source. Loaded once per
  // refresh; drives the highlight set (which sentence indices have
  // facts) and the facts-by-sentence map used by the click modal.
  const [refsData] = createResource(
    () => ({ slug: params.slug, sourceID: params.sourceID, key: refreshKey() }),
    async ({ slug, sourceID }) => {
      if (!slug || !sourceID) return [];
      try {
        return await api.listSourceReferences(slug, sourceID);
      } catch {
        return [];
      }
    },
  );

  // Set<number> of sentence_index values that have at least one
  // fact. Passed to SourceBody to mark highlighted spans.
  const highlightIndices = createMemo(() => {
    const refs = refsData() || [];
    if (!refs.length) return null;
    const set = new Set();
    for (const r of refs) set.add(r.sentence_index);
    return set;
  });

  // Map<sentenceIndex, FactReference[]> for the click modal. Built
  // once per refs load; the click handler looks up the active
  // sentence's facts.
  const factsBySentence = createMemo(() => {
    const refs = refsData() || [];
    const map = new Map();
    for (const r of refs) {
      const arr = map.get(r.sentence_index);
      if (arr) arr.push(r);
      else map.set(r.sentence_index, [r]);
    }
    return map;
  });

  // Map<sentenceIndex, count> for the per-sentence fact count badge.
  // Drives the `<sup class="okt-sentence-count">` injected next to
  // each highlighted sentence so the reader can see at a glance how
  // many facts came from that sentence.
  const factCounts = createMemo(() => {
    const map = new Map();
    for (const [idx, arr] of factsBySentence()) map.set(idx, arr.length);
    return map;
  });

  const [activeSentence, setActiveSentence] = createSignal(null);
  const onSentenceClick = (idx) => setActiveSentence(idx);
  const activeFacts = () =>
    activeSentence() == null ? [] : factsBySentence().get(activeSentence()) || [];
  const closeModal = () => setActiveSentence(null);

  const onFactsSearch = (q) => {
    setFactsSearch(q);
    setFactsOffset(0);
  };

  const refresh = () => {
    setRefreshKey(refreshKey() + 1);
  };

  const handleProcess = async () => {
    const slug = params.slug;
    const sourceID = params.sourceID;
    if (!slug || !sourceID) return;
    setProcessing(true);
    setProcessAlert(null);
    try {
      await api.processSource(slug, sourceID);
      setProcessAlert({
        variant: "success",
        message: "Decomposition job queued. Refresh to see results.",
      });
    } catch (err) {
      setProcessAlert({ variant: "error", message: err.message });
    } finally {
      setProcessing(false);
    }
  };

  const source = () => data()?.source;
  const isFetched = () => source()?.status === "fetched";
  const isProcessed = () => source()?.status === "processed";
  const hasParsedText = () => source()?.parsed_text && source().parsed_text.trim().length > 0;
  const canProcess = () => canUpdate() && isFetched() && hasParsedText() && !isProcessed();

  return (
    <Layout>
      <Show
        when={canRead()}
        fallback={
          <EmptyState
            title="You do not have permission to view this source."
            description="Ask a repository admin to grant you the source:read permission."
          />
        }
      >
        <Show when={!data.loading} fallback={<Loading message="Loading source..." />}>
          <Show
            when={!data.error}
            fallback={<FetchErrorState error={data.error} onRetry={refresh} slug={params.slug} />}
          >
            <Show
              when={source()}
              fallback={
                <EmptyState
                  title="Source not found."
                  description="The source may have been deleted or the id is wrong."
                />
              }
            >
              <ParseEmptyState
                source={source()}
                copy={parseStatusCopy}
                slug={params.slug}
                onRetry={refresh}
              >
                <SourceDetailContent
                  source={() => source()}
                  images={() => data().images}
                  slug={params.slug}
                  sourceID={params.sourceID}
                  error={source()?.error || null}
                  highlightIndices={highlightIndices}
                  factCounts={factCounts}
                  onSentenceClick={onSentenceClick}
                />
              </ParseEmptyState>

              <div class="mt-4 flex items-center gap-3 flex-wrap">
                <Show
                  when={source()?.parse_status === "ok" || source()?.parse_status === "unsupported"}
                >
                  <Button variant="secondary" onClick={refresh} class="text-xs">
                    Refresh
                  </Button>
                </Show>
                <Show when={canProcess()}>
                  <Button
                    variant="primary"
                    onClick={handleProcess}
                    loading={processing()}
                    loadingText="Queuing..."
                    class="text-xs"
                  >
                    Process (extract facts)
                  </Button>
                </Show>
                <Show when={isProcessed()}>
                  <Badge variant="green">Processed</Badge>
                </Show>
              </div>

              <Show when={processAlert()}>
                <div class="mt-3">
                  <Alert
                    variant={processAlert().variant}
                    message={processAlert().message}
                    onDismiss={() => setProcessAlert(null)}
                  />
                </div>
              </Show>

              <SourceFacts
                facts={factsData}
                slug={params.slug}
                onRefresh={refetchFacts}
                search={factsSearch}
                onSearch={onFactsSearch}
                offset={factsOffset}
                onOffsetChange={setFactsOffset}
                total={() => factsData()?.total || 0}
                limit={factsPageLimit}
              />

              <SourceSentenceModal
                open={activeSentence() != null}
                onClose={closeModal}
                sentenceIndex={activeSentence()}
                facts={activeFacts()}
                slug={params.slug}
              />
            </Show>
          </Show>
        </Show>
      </Show>
    </Layout>
  );
}
