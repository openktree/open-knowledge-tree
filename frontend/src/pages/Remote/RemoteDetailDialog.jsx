import { Show, For, createSignal, onCleanup, onMount } from "solid-js";
import { api } from "../../services/api";
import Button from "../../components/Button";
import Alert from "../../components/Alert";
import Badge from "../../components/Badge";

// RemoteDetailDialog is the modal opened from a Remote row click.
// It shows the full source metadata, the list of available
// decomposition models, and lets the user expand a model to
// fetch + browse its facts / concepts / links on demand. A raw
// JSON toggle is always available for troubleshooting.
//
// The dialog fetches the SourcePackage lazily on open
// (api.getRemoteSource) and caches per-model decompositions in
// a local signal so re-expanding a card doesn't refetch.
//
// Props:
//   - source: the row from listRemoteSources (RemoteSourceMeta +
//             exists flag). Only used for the title and the Pull
//             button — the dialog fetches its own metadata.
//   - slug: current repo slug, used for the proxy endpoints.
//   - pullingID: accessor for the currently-pulling row id (the
//             row's Pull button uses this; we mirror it so the
//             dialog's Pull button stays in sync).
//   - onPull: callback that fires when the dialog's Pull button
//             is clicked. Same shape as the row's onPull.
//   - onClose: close the dialog.
export default function RemoteDetailDialog(props) {
  const [detail, setDetail] = createSignal(null);
  const [loading, setLoading] = createSignal(false);
  const [error, setError] = createSignal(null);
  const [pulling, setPulling] = createSignal(false);
  const [pullResult, setPullResult] = createSignal(null);
  // modelID -> { data, loading, error, expanded, rawOpen }
  const [decomps, setDecomps] = createSignal({});

  const onKey = (e) => {
    if (e.key === "Escape") props.onClose?.();
  };

  onMount(async () => {
    document.addEventListener("keydown", onKey);
    document.body.style.overflow = "hidden";
    await loadDetail();
  });
  onCleanup(() => {
    document.removeEventListener("keydown", onKey);
    document.body.style.overflow = "";
  });

  const fetchDecompData = async (model) => {
    // When the registry provides a presigned S3 URL, fetch the
    // decomposition package directly from object storage — no need
    // to proxy through the backend. Falls back to the backend
    // proxy endpoint when no presigned URL is available (e.g.
    // local dev with filesystem storage).
    if (model.presigned_url) {
      const res = await fetch(model.presigned_url);
      if (!res.ok) throw new Error(`S3 fetch failed: ${res.status}`);
      return res.json();
    }
    return api.getRemoteDecomposition(props.slug, props.source.id, model.model_id);
  };

  const loadDetail = async () => {
    setLoading(true);
    setError(null);
    try {
      const pkg = await api.getRemoteSource(props.slug, props.source.id);
      setDetail(pkg);
      // Auto-fetch every decomposition's contents in parallel so
      // the cards are populated as soon as the dialog opens.
      const models = pkg?.decompositions || [];
      if (models.length > 0) {
        setDecomps(Object.fromEntries(
          models.map((m) => [m.model_id, { data: null, loading: true, error: null, expanded: false, rawOpen: false }]),
        ));
        await Promise.all(models.map(async (m) => {
          try {
            const decompPkg = await fetchDecompData(m);
            setDecomps((prev) => ({
              ...prev,
              [m.model_id]: { data: decompPkg, loading: false, error: null, expanded: true, rawOpen: false },
            }));
          } catch (err) {
            setDecomps((prev) => ({
              ...prev,
              [m.model_id]: { data: null, loading: false, error: err.message, expanded: true, rawOpen: false },
            }));
          }
        }));
      }
    } catch (err) {
      setError(err.message);
    } finally {
      setLoading(false);
    }
  };

  const fetchDecomp = async (modelID) => {
    const cur = decomps();
    if (cur[modelID]?.data) {
      // already fetched — just toggle expanded
      setDecomps({ ...cur, [modelID]: { ...cur[modelID], expanded: !cur[modelID].expanded } });
      return;
    }
    const model = (detail()?.decompositions || []).find((d) => d.model_id === modelID);
    setDecomps({ ...cur, [modelID]: { data: null, loading: true, error: null, expanded: true, rawOpen: false } });
    try {
      const pkg = await fetchDecompData(model || { model_id: modelID });
      const after = decomps();
      setDecomps({ ...after, [modelID]: { data: pkg, loading: false, error: null, expanded: true, rawOpen: false } });
    } catch (err) {
      const after = decomps();
      setDecomps({ ...after, [modelID]: { data: null, loading: false, error: err.message, expanded: true, rawOpen: false } });
    }
  };

  const toggleDecomp = (modelID) => {
    const cur = decomps();
    const d = cur[modelID];
    if (!d) {
      fetchDecomp(modelID);
      return;
    }
    setDecomps({ ...cur, [modelID]: { ...d, expanded: !d.expanded } });
  };

  const toggleRaw = (modelID) => {
    const cur = decomps();
    const d = cur[modelID];
    if (!d) return;
    setDecomps({ ...cur, [modelID]: { ...d, rawOpen: !d.rawOpen } });
  };

  const handlePull = async () => {
    setPulling(true);
    setPullResult(null);
    try {
      const result = await props.onPull(props.source);
      setPullResult({ variant: "success", message: `Imported ${result.imported_facts} facts, ${result.imported_concepts} concepts` });
    } catch (err) {
      setPullResult({ variant: "error", message: err.message });
    } finally {
      setPulling(false);
    }
  };

  const decompList = () => detail()?.decompositions || [];

  return (
    <div
      class="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4"
      onClick={() => props.onClose?.()}
    >
      <div
        class="bg-white dark:bg-gray-800 rounded-lg shadow-xl max-w-3xl w-full max-h-[85vh] flex flex-col"
        onClick={(e) => e.stopPropagation()}
      >
        <div class="px-5 py-3 border-b dark:border-gray-700 flex items-center justify-between">
          <h3 class="text-sm font-semibold text-gray-800 dark:text-gray-200 truncate min-w-0 pr-3">
            {props.source.title || props.source.url || props.source.id}
          </h3>
          <button
            class="text-gray-400 hover:text-gray-600 dark:hover:text-gray-200 text-lg leading-none shrink-0"
            onClick={() => props.onClose?.()}
            aria-label="Close"
          >
            ×
          </button>
        </div>
        <div class="px-5 py-4 overflow-y-auto text-sm text-gray-700 dark:text-gray-300 space-y-5">
          <Show when={loading()}>
            <div class="flex items-center gap-2 text-gray-500 dark:text-gray-400">
              <svg class="animate-spin h-4 w-4" viewBox="0 0 24 24" fill="none">
                <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4" />
                <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z" />
              </svg>
              <span>Loading source detail...</span>
            </div>
          </Show>
          <Alert variant={error() ? "error" : null} message={error()} />
          <Show when={!loading() && !error()}>
            <SourceMetadata source={props.source} detail={detail} />
            <Decompositions
              decomps={decomps}
              list={decompList()}
              onToggle={toggleDecomp}
              onRaw={toggleRaw}
              onFetch={fetchDecomp}
            />
            <RawSourceJSON detail={detail} />
          </Show>
        </div>
        <div class="px-5 py-3 border-t dark:border-gray-700 flex items-center justify-end gap-3">
          <Alert
            variant={pullResult()?.variant}
            message={pullResult()?.message}
            onDismiss={() => setPullResult(null)}
          />
          <Button variant="secondary" onClick={() => props.onClose?.()}>Close</Button>
          <Button
            variant="primary"
            onClick={handlePull}
            loading={pulling() || props.pullingID?.() === props.source.id}
            loadingText="Pulling..."
            disabled={props.source.exists && pulling()}
          >
            {props.source.exists ? "Re-sync" : "Pull to repository"}
          </Button>
        </div>
      </div>
    </div>
  );
}

function SourceMetadata(props) {
  const s = () => props.source;
  const d = () => props.detail?.source || {};
  const rows = () => [
    ["ID", s().id],
    ["Title", s().title || d().title],
    ["URL", s().url],
    ["DOI", s().doi],
    ["SHA256", d().sha256],
    ["S3 key", d().s3_key],
    ["Repo ID", s().repo_id || d().repo_id],
    ["Created at", s().created_at],
    ["Updated at", s().updated_at],
  ];
  return (
    <section>
      <h4 class="text-sm font-semibold text-gray-800 dark:text-gray-200 mb-2">Source metadata</h4>
      <dl class="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-xs">
        <For each={rows()}>
          {([k, v]) => (
            <Show when={v}>
              <dt class="text-gray-500 dark:text-gray-400">{k}</dt>
              <dd class="font-mono text-gray-800 dark:text-gray-200 break-all min-w-0">{v}</dd>
            </Show>
          )}
        </For>
      </dl>
    </section>
  );
}

function Decompositions(props) {
  return (
    <section>
      <h4 class="text-sm font-semibold text-gray-800 dark:text-gray-200 mb-2">
        Decompositions ({props.list.length})
      </h4>
      <Show when={props.list.length > 0} fallback={
        <p class="text-xs text-gray-500 dark:text-gray-400 italic">
          No decompositions available for this source.
        </p>
      }>
        <div class="space-y-2">
          <For each={props.list}>
            {(d) => <DecompCard entry={props.decomps()[d.model_id]} decomp={d} onToggle={() => props.onToggle(d.model_id)} onRaw={() => props.onRaw(d.model_id)} onFetch={() => props.onFetch(d.model_id)} />}
          </For>
        </div>
      </Show>
    </section>
  );
}

function DecompCard(props) {
  const e = () => props.entry;
  return (
    <div class="border border-gray-200 dark:border-gray-700 rounded">
      <div class="p-2.5 flex items-center justify-between gap-2">
        <button type="button" class="flex-1 min-w-0 text-left flex items-center gap-2" onClick={props.onToggle}>
          <span class="text-gray-400 dark:text-gray-500 text-xs inline-block w-3 transition-transform" classList={{ "rotate-90": e()?.expanded }}>▶</span>
          <span class="font-mono text-xs text-gray-800 dark:text-gray-200 truncate">{props.decomp.model_id}</span>
          <span class="text-xs text-gray-500 dark:text-gray-400 shrink-0">{props.decomp.fact_count} facts</span>
          <Show when={props.decomp.embedding_model}>
            <span class="text-xs text-gray-400 dark:text-gray-500 shrink-0 font-mono">emb: {props.decomp.embedding_model}</span>
          </Show>
          <Badge variant={props.decomp.has_embeddings ? "green" : "gray"}>
            {props.decomp.has_embeddings ? "embed" : "no embed"}
          </Badge>
        </button>
        <Show when={!e()?.data && !e()?.loading}>
          <Button variant="secondary" onClick={props.onFetch} class="text-xs">Fetch</Button>
        </Show>
        <Show when={e()?.loading}>
          <span class="flex items-center gap-1 text-xs text-gray-500 dark:text-gray-400">
            <svg class="animate-spin h-3 w-3" viewBox="0 0 24 24" fill="none">
              <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4" />
              <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z" />
            </svg>
            Loading...
          </span>
        </Show>
      </div>
      <Show when={e()?.expanded}>
        <div class="border-t border-gray-200 dark:border-gray-700 p-2.5 space-y-3">
          <Alert variant={e()?.error ? "error" : null} message={e()?.error} />
          <Show when={e()?.data}>
            <FactsList facts={e().data.facts || []} />
            <ConceptsList concepts={e().data.concepts || []} />
            <LinksList links={e().data.links || []} />
            <div>
              <button
                type="button"
                class="text-xs text-blue-600 dark:text-blue-400 hover:underline"
                onClick={props.onRaw}
              >
                {e().rawOpen ? "Hide raw JSON" : "Show raw JSON"}
              </button>
              <Show when={e().rawOpen}>
                <pre class="mt-2 p-2 bg-gray-50 dark:bg-gray-900 text-[10px] font-mono rounded border border-gray-200 dark:border-gray-700 overflow-x-auto whitespace-pre-wrap break-all">
{JSON.stringify(e().data, null, 2)}
                </pre>
              </Show>
            </div>
          </Show>
        </div>
      </Show>
    </div>
  );
}

function FactsList(props) {
  return (
    <div>
      <h5 class="text-xs font-semibold text-gray-700 dark:text-gray-300 mb-1">
        Facts ({props.facts.length})
      </h5>
      <Show when={props.facts.length > 0} fallback={
        <p class="text-xs text-gray-500 dark:text-gray-400 italic">No facts.</p>
      }>
        <ul class="space-y-1.5">
          <For each={props.facts}>
            {(f) => <FactRow fact={f} />}
          </For>
        </ul>
      </Show>
    </div>
  );
}

function FactRow(props) {
  const [open, setOpen] = createSignal(false);
  const long = () => (props.fact.content || "").length > 200;
  return (
    <li class="border border-gray-200 dark:border-gray-700 rounded p-2 bg-gray-50 dark:bg-gray-900/40">
      <p class="text-xs text-gray-800 dark:text-gray-200 whitespace-pre-wrap break-words">
        {long() && !open() ? `${props.fact.content.slice(0, 200)}…` : props.fact.content}
      </p>
      <Show when={long()}>
        <button type="button" class="text-[10px] text-blue-600 dark:text-blue-400 hover:underline" onClick={() => setOpen(!open())}>
          {open() ? "Show less" : "Show more"}
        </button>
      </Show>
      <div class="flex flex-wrap items-center gap-3 mt-1 text-[10px] text-gray-500 dark:text-gray-400 font-mono">
        <Show when={props.fact.sentence_index != null}><span>idx: {props.fact.sentence_index}</span></Show>
        <Show when={props.fact.confidence != null}><span>conf: {Number(props.fact.confidence).toFixed(2)}</span></Show>
        <Show when={props.fact.content_hash}><span>hash: {props.fact.content_hash.slice(0, 12)}…</span></Show>
        <Show when={props.fact.image_url}>
          <a href={props.fact.image_url} target="_blank" rel="noreferrer" class="hover:underline">
            <img src={props.fact.image_url} alt={props.fact.image_caption || ""} class="h-10 inline-block rounded border border-gray-200 dark:border-gray-700 align-middle" />
          </a>
        </Show>
      </div>
    </li>
  );
}

function ConceptsList(props) {
  return (
    <div>
      <h5 class="text-xs font-semibold text-gray-700 dark:text-gray-300 mb-1">
        Concepts ({props.concepts.length})
      </h5>
      <Show when={props.concepts.length > 0} fallback={
        <p class="text-xs text-gray-500 dark:text-gray-400 italic">No concepts.</p>
      }>
        <ul class="space-y-1.5">
          <For each={props.concepts}>
            {(c) => (
              <li class="border border-gray-200 dark:border-gray-700 rounded p-2 bg-gray-50 dark:bg-gray-900/40 text-xs">
                <div class="flex flex-wrap items-center gap-2">
                  <span class="font-semibold text-gray-800 dark:text-gray-200">{c.canonical_name}</span>
                  <Show when={c.context}><span class="text-gray-500 dark:text-gray-400">@ {c.context}</span></Show>
                  <Show when={c.ontology_class}><Badge variant="blue">{c.ontology_class}</Badge></Show>
                </div>
                <Show when={c.aliases && c.aliases.length > 0}>
                  <div class="mt-1 flex flex-wrap gap-1">
                    <For each={c.aliases}>
                      {(a) => <Badge variant="gray">{a}</Badge>}
                    </For>
                  </div>
                </Show>
              </li>
            )}
          </For>
        </ul>
      </Show>
    </div>
  );
}

function LinksList(props) {
  return (
    <Show when={props.links.length > 0}>
      <div>
        <h5 class="text-xs font-semibold text-gray-700 dark:text-gray-300 mb-1">
          Fact→Concept Links ({props.links.length})
        </h5>
        <ul class="space-y-1 text-[10px] font-mono text-gray-600 dark:text-gray-400">
          <For each={props.links}>
            {(l) => (
              <li>
                {l.fact_content_hash?.slice(0, 12) || "?"}… → {l.concept_name}
                <Show when={l.concept_context}> <span class="text-gray-400">@{l.concept_context}</span></Show>
              </li>
            )}
          </For>
        </ul>
      </div>
    </Show>
  );
}

function RawSourceJSON(props) {
  const [open, setOpen] = createSignal(false);
  return (
    <section>
      <button
        type="button"
        class="text-xs text-blue-600 dark:text-blue-400 hover:underline"
        onClick={() => setOpen(!open())}
      >
        {open() ? "Hide source JSON" : "Show source JSON"}
      </button>
      <Show when={open()}>
        <pre class="mt-2 p-2 bg-gray-50 dark:bg-gray-900 text-[10px] font-mono rounded border border-gray-200 dark:border-gray-700 overflow-x-auto whitespace-pre-wrap break-all">
{JSON.stringify(props.detail, null, 2)}
        </pre>
      </Show>
    </section>
  );
}
