import { useNavigate } from "@solidjs/router";
import { createEffect, createResource, createSignal, onCleanup, Show, untrack } from "solid-js";
import Badge from "../../components/Badge";
import Card from "../../components/Card";
import EmptyState from "../../components/EmptyState";
import { renderMarkdown } from "../../lib/markdown";
import { normalizeCitations, normalizeImageCitations } from "../../lib/normalizeCitations";
import { resolveDefinitionImages, revokeBlobUrls } from "../../lib/resolveDefinitionImages";
import { api } from "../../services/api";

// DefinitionPanel renders the concept's "definition" — the single
// authoritative synthesis the synthesize_concept worker produces for
// the concept's canonical-name group, folding ALL the group's summary
// slices into one markdown body. The definition is group-scoped (one
// row per (repository_id, lower(canonical_name))), so it is the same
// regardless of which context tab is active; the parent passes the
// first context's concept_id (any concept_id in the group resolves to
// the same definition via the endpoint).
//
// The definition markdown carries two citation shapes the frontend
// rewrites before micromark renders the body:
//   - [text](<fact:fact_id>)   text citations -> numbered fact-detail links
//     (reuses the shared normalizeCitations helper).
//   - [name](<concept:concept_id>) concept citations -> concept-detail links
//     (also handled by normalizeCitations; kind prefix routes them).
//   - ![alt](<fact:fact_id>)   image citations -> ![alt](renderableUrl)
//     using the eager-loaded images array (storage URLs are resolved
//     to authenticated blob URLs via resolveDefinitionImages).
//
// Props:
//   - slug: repo slug (for fact-detail hrefs + storage image fetch)
//   - conceptID: any concept_id in the group (the endpoint resolves
//     it to the canonical-name group).
export default function DefinitionPanel(props) {
  const slug = () => props.slug;
  const conceptID = () => props.conceptID;
  const navigate = useNavigate();

  const [refreshKey, setRefreshKey] = createSignal(0);
  const [collapsed, setCollapsed] = createSignal(true);
  const [blobUrls, setBlobUrls] = createSignal([]);
  const [renderedHtml, setRenderedHtml] = createSignal("");
  let defEl = null;

  const [defData, { refetch }] = createResource(
    () => ({ slug: slug(), conceptID: conceptID(), key: refreshKey() }),
    async ({ slug, conceptID }) => {
      if (!slug || !conceptID) return null;
      try {
        return await api.getConceptDefinition(slug, conceptID);
      } catch (err) {
        if (err?.status === 404) return { notFound: true };
        return { error: err };
      }
    },
  );

  const synthesis = () => defData()?.synthesis || null;
  const images = () => defData()?.images || [];
  const notFound = () => !!defData()?.notFound;
  const errored = () => !!defData()?.error;

  // Re-render the definition markdown whenever the resource settles.
  // Image URLs are resolved to blob URLs (revoking the previous set)
  // before normalizing citations + micromark rendering.
  createEffect(() => {
    const data = defData();
    if (!data || data.error || data.notFound) {
      setRenderedHtml("");
      return;
    }
    const syn = data.synthesis;
    if (!syn || !syn.content) {
      setRenderedHtml("");
      return;
    }
    (async () => {
      // Read the previous blob URLs without tracking them — otherwise
      // setBlobUrls() below would retrigger this effect and spin forever.
      const prev = untrack(blobUrls);
      revokeBlobUrls(prev);
      const { map, blobUrls: newBlobs } = await resolveDefinitionImages(data.images || []);
      setBlobUrls(newBlobs);
      let md = normalizeImageCitations(syn.content, map);
      md = normalizeCitations(md, slug());
      setRenderedHtml(renderMarkdown(md));
    })();
  });

  onCleanup(() => revokeBlobUrls(blobUrls()));

  const onDefinitionClick = (e) => {
    const a = e.target.closest("a");
    if (!a) return;
    const href = a.getAttribute("href") || "";
    if (
      href.startsWith("/") &&
      (/\/facts\/[0-9a-fA-F-]{36}/.test(href) || /\/concepts\/[0-9a-fA-F-]{36}/.test(href))
    ) {
      e.preventDefault();
      navigate(href);
    }
  };

  const toggleCollapse = () => setCollapsed((c) => !c);

  // Keep the DOM in sync with renderedHtml: the ref-based approach
  // only fires once on mount, so we update innerHTML reactively here
  // whenever the rendered HTML changes (e.g. after async image
  // resolution completes).
  createEffect(() => {
    const html = renderedHtml();
    if (defEl) defEl.innerHTML = html;
  });

  return (
    <Card>
      <div class="flex items-center justify-between gap-3 flex-wrap">
        <button
          type="button"
          onClick={toggleCollapse}
          class="flex items-center gap-2 text-left flex-wrap group"
          aria-expanded={!collapsed()}
          aria-controls="concept-definition-body"
        >
          <span class="text-lg font-semibold dark:text-white">Definition</span>
          <Show when={synthesis()}>
            <Badge variant="blue">synthesized</Badge>
          </Show>
          <span
            class="text-xs text-gray-400 dark:text-gray-500 transition-transform group-hover:text-gray-600 dark:group-hover:text-gray-300"
            style={collapsed() ? "transform: rotate(-90deg)" : ""}
          >
            ▾
          </span>
        </button>
        <button
          type="button"
          class="text-xs px-2 py-1 rounded border border-gray-300 dark:border-gray-600 text-gray-600 dark:text-gray-300 hover:bg-gray-50 dark:hover:bg-gray-800"
          onClick={refetch}
        >
          Refresh
        </button>
      </div>

      <Show when={!collapsed()}>
        <div id="concept-definition-body" class="mt-4">
          <Show
            when={!notFound() && !errored()}
            fallback={
              <Show
                when={notFound()}
                fallback={
                  <EmptyState
                    title="Couldn't load the definition."
                    description={
                      defData()?.error?.message ||
                      "Something went wrong fetching the definition. Try refreshing."
                    }
                  />
                }
              >
                <EmptyState
                  title="No definition yet."
                  description="The definition is generated by the synthesize_concept task once enough summaries exist for this concept. Check back after the next synthesis pass."
                />
              </Show>
            }
          >
            <Show when={synthesis()}>
              <div class="border rounded dark:border-gray-700 p-4">
                <div class="flex items-center gap-2 mb-3 flex-wrap">
                  <Show when={synthesis().model}>
                    <span class="text-xs text-gray-400 dark:text-gray-500">
                      {synthesis().model}
                    </span>
                  </Show>
                  <Show when={synthesis().updated_at}>
                    <span class="text-xs text-gray-400 dark:text-gray-500">
                      updated {new Date(synthesis().updated_at).toLocaleString()}
                    </span>
                  </Show>
                  <Show when={images().length > 0}>
                    <Badge variant="gray">
                      {images().length} image{images().length === 1 ? "" : "s"}
                    </Badge>
                  </Show>
                </div>
                <div
                  class="prose dark:prose-invert max-w-none text-sm text-gray-800 dark:text-gray-200 leading-relaxed"
                  ref={(el) => {
                    defEl = el;
                    if (el) el.innerHTML = renderedHtml();
                  }}
                  onClick={onDefinitionClick}
                />
              </div>
            </Show>
          </Show>
        </div>
      </Show>
    </Card>
  );
}
