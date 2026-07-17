import { createSignal, Show, onCleanup } from "solid-js";
import Button from "../../components/Button";
import { api } from "../../services/api";
import CitedView from "../../components/CitedView";

/**
 * Body of the SourceDetail page. Renders the parsed Markdown as
 * styled HTML with each sentence wrapped in a highlightable span.
 * Sentences that have at least one fact get the
 * `okt-sentence--has-facts` class and a click handler.
 *
 * Delegates the markdown rendering + sentence wrapping to the
 * shared CitedView component so the same interaction pattern
 * works for sources and reports.
 *
 * Props:
 *   - source: accessor returning the source row
 *   - slug, sourceID: route params for the serving endpoint
 *   - highlightIndices: accessor returning Set<number> | null
 *   - factCounts: accessor returning Map<number, number> | null
 *   - onSentenceClick: (sentenceIndex: number) => void
 */
export default function SourceBody(props) {
  const [pdfUrl, setPdfUrl] = createSignal(null);
  const [pdfFailed, setPdfFailed] = createSignal(false);

  const markdown = () => props.source()?.parsed_markdown || "";
  const text = () => props.source()?.parsed_text || "";
  const hasBody = () => markdown().trim().length > 0 || text().trim().length > 0;
  const hasStoredBody = () => !!props.source()?.storage_key;

  const fetchPdfUrl = () => {
    if (pdfUrl() || pdfFailed() || !hasStoredBody()) return;
    api
      .getSourceBody(props.slug, props.sourceID)
      .then((url) => {
        if (url) setPdfUrl(url);
        else setPdfFailed(true);
      })
      .catch(() => setPdfFailed(true));
  };

  onCleanup(() => {
    if (pdfUrl()) URL.revokeObjectURL(pdfUrl());
  });

  return (
    <section class="space-y-3">
      <div class="flex items-center justify-between">
        <h2 class="text-sm font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wide">
          Extracted content
        </h2>
        <Show when={hasStoredBody()}>
          <Show
            when={pdfUrl()}
            fallback={
              <Button variant="link" class="text-xs" onClick={fetchPdfUrl}>
                View original PDF
              </Button>
            }
          >
            <a
              href={pdfUrl()}
              target="_blank"
              rel="noopener noreferrer"
              class="text-xs text-blue-600 dark:text-blue-400 hover:underline"
            >
              Open original PDF
            </a>
          </Show>
        </Show>
      </div>

      <Show
        when={hasBody()}
        fallback={
          <p class="text-sm text-gray-500 dark:text-gray-400 italic">
            No readable text was extracted from this source.
          </p>
        }
      >
        <Show
          when={markdown().trim().length > 0}
          fallback={
            <article class="prose dark:prose-invert max-w-none whitespace-pre-wrap text-sm text-gray-800 dark:text-gray-200 leading-relaxed">
              {text()}
            </article>
          }
        >
          <CitedView
            markdown={markdown()}
            highlightIndices={props.highlightIndices?.()}
            factCounts={props.factCounts?.()}
            onSentenceClick={props.onSentenceClick}
          />
        </Show>
      </Show>
    </section>
  );
}