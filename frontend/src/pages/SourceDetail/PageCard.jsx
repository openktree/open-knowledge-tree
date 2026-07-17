import { createSignal, Show, onCleanup } from "solid-js";
import Badge from "../../components/Badge";
import { api } from "../../services/api";

/**
 * One PDF page-render card. Renders a real thumbnail when the page
 * render has been mirrored to storage; otherwise a placeholder card
 * with the page number, dimensions, and recorded byte size. Page
 * renders have no remote URL, so un-mirrored rows have no fallback.
 *
 * The blob URL is fetched lazily on first paint (via the ref
 * callback) and revoked on cleanup to avoid leaking blob memory.
 *
 * Props:
 *   - image: the source_images row (kind === "page")
 *   - slug, sourceID: route params for the serving endpoint
 */
export default function PageCard(props) {
  const stored = () => !!props.image.storage_key;
  const [blobUrl, setBlobUrl] = createSignal(null);
  const [failed, setFailed] = createSignal(false);

  const fetchBlob = () => {
    if (!stored() || blobUrl() || failed()) return;
    api
      .getSourceImage(props.slug, props.sourceID, props.image.id)
      .then((url) => {
        if (url) setBlobUrl(url);
        else setFailed(true);
      })
      .catch(() => setFailed(true));
  };

  onCleanup(() => {
    if (blobUrl()) URL.revokeObjectURL(blobUrl());
  });

  const dim = () => {
    const w = props.image.width;
    const h = props.image.height;
    if (w && h) return `${w} × ${h}`;
    return "unknown size";
  };
  const sizeKB = () => (props.image.bytes ? Math.round(props.image.bytes / 1024) : null);

  return (
    <div class="border rounded dark:border-gray-700 bg-gray-50 dark:bg-gray-900 overflow-hidden">
      <Show
        when={stored() && !failed()}
        fallback={
          <div class="aspect-[3/4] flex items-center justify-center bg-white dark:bg-gray-800 text-gray-400 dark:text-gray-600">
            <span class="text-2xl font-semibold">p.{props.image.page_number}</span>
          </div>
        }
      >
        <Show
          when={blobUrl()}
          fallback={
            <div
              class="aspect-[3/4] flex items-center justify-center bg-white dark:bg-gray-800 text-gray-400 dark:text-gray-600"
              ref={(el) => { if (el) fetchBlob(); }}
            >
              <span class="text-2xl font-semibold">p.{props.image.page_number}</span>
            </div>
          }
        >
          <a
            href={blobUrl()}
            target="_blank"
            rel="noopener noreferrer"
            class="block aspect-[3/4] overflow-hidden bg-white dark:bg-gray-800"
            title={`Page ${props.image.page_number}`}
          >
            <img
              src={blobUrl()}
              alt={`Page ${props.image.page_number}`}
              loading="lazy"
              class="w-full h-full object-contain"
            />
          </a>
        </Show>
      </Show>
      <div class="p-2 space-y-1">
        <div class="flex items-center gap-1 flex-wrap">
          <Badge variant="gray">page {props.image.page_number}</Badge>
        </div>
        <p class="text-[10px] font-mono text-gray-500 dark:text-gray-400">{dim()}</p>
        <Show when={sizeKB() !== null}>
          <p class="text-[10px] font-mono text-gray-500 dark:text-gray-400">
            {sizeKB()} KB
          </p>
        </Show>
      </div>
    </div>
  );
}