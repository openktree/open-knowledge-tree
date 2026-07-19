import { For } from "solid-js";
import PageCard from "./PageCard";

/**
 * PDF page-render gallery. Each row is one page rendered to PNG by
 * the parser. Each card renders a real thumbnail when the row has
 * been mirrored to storage; otherwise a placeholder card with the
 * page number, dimensions, and recorded byte size (page renders
 * have no remote URL, so there is no fallback).
 *
 * Props:
 *   - images: accessor returning the page-render image rows
 *   - slug, sourceID: route params for the serving endpoint
 */
export default function PageGallery(props) {
  return (
    <div class="space-y-3">
      <h2 class="text-sm font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wide">
        PDF page renders ({props.images().length})
      </h2>
      <p class="text-xs text-gray-500 dark:text-gray-400">
        Per-page renders of the source PDF, served from the storage backend. Pages that have not
        been mirrored yet show a placeholder card with the page number, dimensions, and recorded
        byte size.
      </p>
      <div class="grid grid-cols-2 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5 gap-3">
        <For each={props.images()}>
          {(img) => <PageCard image={img} slug={props.slug} sourceID={props.sourceID} />}
        </For>
      </div>
    </div>
  );
}
