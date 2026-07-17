import { createResource, createSignal, Show, For } from "solid-js";
import { api } from "../../services/api";
import Button from "../../components/Button";
import CollapsibleSection from "../../components/CollapsibleSection";
import SearchInput from "../../components/SearchInput";
import Pagination from "../../components/Pagination";
import EmptyState from "../../components/EmptyState";
import Loading from "../../components/Loading";
import UploadSourcePanel from "../../components/UploadSourcePanel";
import AddExistingSourcePicker from "./AddExistingSourcePicker";
import InvestigationSourceRow from "./InvestigationSourceRow";
import SearchAndRetrievePanel from "./SearchAndRetrievePanel";

export default function InvestigationSourcesPhase(props) {
  const [search, setSearch] = createSignal("");
  const [offset, setOffset] = createSignal(0);
  const [refreshKey, setRefreshKey] = createSignal(0);
  const [processingID, setProcessingID] = createSignal("");

  const [srcData, { refetch }] = createResource(
    () => [props.slug, props.invID, search(), offset(), refreshKey()],
    async ([s, id, q, off]) => {
      if (!s || !id) return { data: [], total: 0, limit: 100, offset: 0 };
      try {
        return await api.listInvestigationSources(s, id, { q, offset: off });
      } catch (err) {
        props.onAlert?.({ variant: "error", message: err.message });
        return { data: [], total: 0, limit: 100, offset: 0 };
      }
    }
  );

  const sources = () => srcData()?.data || [];
  const total = () => srcData()?.total || 0;
  const limit = () => srcData()?.limit || 100;

  const onSearch = (q) => {
    setSearch(q);
    setOffset(0);
  };

  const handleProcess = async (src) => {
    setProcessingID(src.id);
    try {
      await api.processSource(props.slug, src.id);
      props.onAlert?.({ variant: "success", message: "Decomposition job queued for " + (src.parsed_title || src.url) });
      setRefreshKey((k) => k + 1);
    } catch (err) {
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setProcessingID("");
    }
  };

  return (
    <div class="space-y-6">
      <SearchAndRetrievePanel
        slug={props.slug}
        invID={props.invID}
        onSourceLinked={() => setRefreshKey((k) => k + 1)}
      />
      <CollapsibleSection
        title="Upload a source"
        subtitle="Upload a file (PDF, HTML, Markdown, TXT) or paste raw text. Parsed and decomposed into facts automatically."
        defaultOpen={false}
      >
        <UploadSourcePanel
          slug={props.slug}
          invID={props.invID}
          onDone={() => setRefreshKey((k) => k + 1)}
          bare
        />
      </CollapsibleSection>
      <AddExistingSourcePicker
        slug={props.slug}
        invID={props.invID}
        onAdded={() => setRefreshKey((k) => k + 1)}
      />
      <CollapsibleSection
        title="Sources"
        subtitle={`${total()} source${total() === 1 ? "" : "s"} in this investigation`}
        headerRight={
          <>
            <SearchInput placeholder="Search sources..." onSearch={onSearch} />
            <Button variant="secondary" onClick={refetch}>Refresh</Button>
          </>
        }
      >
        <Show
          when={!srcData.loading}
          fallback={<Loading message="Loading sources..." />}
        >
          <Show
            when={sources().length > 0}
            fallback={
              <EmptyState
                title="No sources in this investigation yet"
                description="Use the search panel above to fetch new sources, or expand 'Add an existing source' to link one already in the repository."
              />
            }
          >
            <Show when={total() > limit()}>
              <Pagination
                total={total()}
                limit={limit()}
                offset={offset()}
                onOffsetChange={setOffset}
              />
              <p class="text-xs text-gray-500 dark:text-gray-400 mt-3">
                Showing {offset() + 1}–{Math.min(offset() + limit(), total())} of {total().toLocaleString()}
              </p>
            </Show>
            <div class="space-y-2 mt-3">
              <For each={sources()}>
                {(src) => (
                  <InvestigationSourceRow
                    slug={props.slug}
                    invID={props.invID}
                    source={src}
                    onRemoved={() => setRefreshKey((k) => k + 1)}
                    onAlert={props.onAlert}
                    onProcess={handleProcess}
                    processing={processingID() === src.id}
                  />
                )}
              </For>
            </div>
            <Show when={total() > limit()}>
              <Pagination
                total={total()}
                limit={limit()}
                offset={offset()}
                onOffsetChange={setOffset}
              />
            </Show>
          </Show>
        </Show>
      </CollapsibleSection>
    </div>
  );
}