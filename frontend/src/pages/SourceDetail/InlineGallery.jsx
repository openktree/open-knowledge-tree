import { For, Show } from "solid-js";
import { INLINE_IMAGE_PREVIEW_LIMIT } from "./constants";
import InlineCard from "./InlineCard";

/**
 * Inline image gallery. Renders inline <img> URLs extracted from
 * HTML sources. Each card handles its own blob-URL fetching from
 * the authenticated serving endpoint when the row has been
 * mirrored; otherwise the card falls back to the remote `url`.
 *
 * Props:
 *   - images: accessor returning the inline image rows
 *   - showAll, setShowAll: pagination signals owned by the parent
 *   - slug, sourceID: route params for the serving endpoint
 */
export default function InlineGallery(props) {
  const visible = () =>
    props.showAll() ? props.images() : props.images().slice(0, INLINE_IMAGE_PREVIEW_LIMIT);
  const overflow = () => Math.max(0, props.images().length - INLINE_IMAGE_PREVIEW_LIMIT);
  const altCount = () =>
    props.images().filter((i) => (i.alt_text || "").trim().length > 0).length;

  return (
    <div class="space-y-3">
      <div class="flex items-baseline justify-between gap-3">
        <h2 class="text-sm font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wide">
          Inline images ({props.images().length})
        </h2>
        <p class="text-xs text-gray-500 dark:text-gray-400">
          {altCount()} of {props.images().length} have alt text
        </p>
      </div>
      <div class="grid grid-cols-2 sm:grid-cols-3 md:grid-cols-4 gap-3">
        <For each={visible()}>
          {(img) => (
            <InlineCard image={img} slug={props.slug} sourceID={props.sourceID} />
          )}
        </For>
      </div>
      <Show when={overflow() > 0 && !props.showAll()}>
        <button
          class="text-xs text-blue-600 dark:text-blue-400 hover:underline"
          onClick={() => props.setShowAll(true)}
        >
          Show all {props.images().length} images
        </button>
      </Show>
    </div>
  );
}