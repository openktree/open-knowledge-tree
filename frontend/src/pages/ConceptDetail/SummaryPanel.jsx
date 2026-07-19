import { useNavigate } from "@solidjs/router";
import { createResource, createSignal, For, Show } from "solid-js";
import Badge from "../../components/Badge";
import Card from "../../components/Card";
import EmptyState from "../../components/EmptyState";
import { renderMarkdown } from "../../lib/markdown";
import { normalizeCitations } from "../../lib/normalizeCitations";
import { api } from "../../services/api";

// SummaryPanel renders the concept summaries for a single
// (concept, context) pair — the output of the summarize_concepts
// worker. Summaries are incremental: a concept carries N slices
// ordered by sequence_num. The oldest slice covering fewer than
// BatchSize facts stays "open" (is_complete = FALSE) and is
// regenerated as new facts arrive; slices that reached BatchSize
// facts are frozen (is_complete = TRUE). Together they let a reader
// absorb a concept with many facts via a few short summaries instead
// of scrolling 100 fact rows.
//
// The summary content is markdown with fact citations. The LLM was
// prompted to emit [text](<fact:fact_id>) markdown links, but in
// practice it produces a variety of shapes: bare [uuid], [uuid1, uuid2],
// ([<uuid>]), and the canonical [text](<fact:uuid>). normalizeCitations
// rewrites all of them into proper markdown links before micromark
// renders the body, so every citation becomes a clickable link to
// the fact detail route. (The summarizer only emits fact citations;
// concept citations [name](<concept:concept_id>) are a synthesis-only
// concern and are also handled by normalizeCitations when they appear.)
//
// Props:
//   - slug: repo slug (for the fact detail href)
//   - conceptID: the per-context concept_id (same one ContextPanel
//     uses to fetch facts)
//   - collapsible: when true (default), the card collapses to its
//     header bar on toggle; when false, always expanded.

export default function SummaryPanel(props) {
  const slug = () => props.slug;
  const conceptID = () => props.conceptID;
  const navigate = useNavigate();

  const [refreshKey, setRefreshKey] = createSignal(0);
  const [collapsed, setCollapsed] = createSignal(true);
  const [summaryData, { refetch }] = createResource(
    () => ({ slug: slug(), conceptID: conceptID(), key: refreshKey() }),
    async ({ slug, conceptID }) => {
      if (!slug || !conceptID) return { data: [], total: 0 };
      try {
        return await api.listConceptSummaries(slug, conceptID);
      } catch {
        return { data: [], total: 0 };
      }
    },
  );

  const summaries = () => summaryData()?.data || [];
  const total = () => summaryData()?.total || 0;
  const hasSummaries = () => summaries().length > 0;

  // Render one summary's content into safe HTML. Citations are
  // normalized to markdown links at the text level, then micromark
  // renders the body. The result is safe to mount via innerHTML
  // (micromark never passes raw HTML through).
  const renderSummaryHtml = (summary) => {
    const normalized = normalizeCitations(summary.content || "", slug());
    return renderMarkdown(normalized);
  };

  // Intercept clicks on citation links so the Solid router handles
  // the navigation (no full page reload). Only internal fact-detail
  // and concept-detail links are intercepted; external links behave
  // normally.
  const onSummaryClick = (e) => {
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

  return (
    <Card>
      <div class="flex items-center justify-between gap-3 flex-wrap">
        <button
          type="button"
          onClick={toggleCollapse}
          class="flex items-center gap-2 text-left flex-wrap group"
          aria-expanded={!collapsed()}
          aria-controls="concept-summaries-body"
        >
          <span class="text-lg font-semibold dark:text-white">Summaries</span>
          <Badge variant="gray">
            {total().toLocaleString()} slice{total() === 1 ? "" : "s"}
          </Badge>
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
        <div id="concept-summaries-body" class="mt-4">
          <Show
            when={hasSummaries()}
            fallback={
              <EmptyState
                title="No summaries yet."
                description="Summaries are generated incrementally by the summarize_concepts task once enough facts are linked to this concept. Check back after the next summarization pass."
              />
            }
          >
            <div class="space-y-4">
              <For each={summaries()}>
                {(summary) => (
                  <div class="border rounded dark:border-gray-700 p-4">
                    <div class="flex items-center gap-2 mb-3 flex-wrap">
                      <span class="text-xs font-mono text-gray-500 dark:text-gray-400">
                        Slice #{summary.sequence_num}
                      </span>
                      <Show when={summary.is_complete}>
                        <Badge variant="blue">frozen · {summary.fact_count} facts</Badge>
                      </Show>
                      <Show when={!summary.is_complete}>
                        <Badge variant="yellow">open · {summary.fact_count} facts</Badge>
                      </Show>
                      <Show when={summary.model}>
                        <span class="text-xs text-gray-400 dark:text-gray-500">
                          {summary.model}
                        </span>
                      </Show>
                      <Show when={summary.updated_at}>
                        <span class="text-xs text-gray-400 dark:text-gray-500">
                          {summary.is_complete ? "frozen" : "updated"}{" "}
                          {new Date(summary.updated_at).toLocaleString()}
                        </span>
                      </Show>
                    </div>
                    <div
                      class="prose dark:prose-invert max-w-none text-sm text-gray-800 dark:text-gray-200 leading-relaxed"
                      ref={(el) => {
                        if (el) el.innerHTML = renderSummaryHtml(summary);
                      }}
                      onClick={onSummaryClick}
                    />
                  </div>
                )}
              </For>
            </div>
          </Show>
        </div>
      </Show>
    </Card>
  );
}
