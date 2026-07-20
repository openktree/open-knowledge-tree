import { createMemo, createSignal, onCleanup } from "solid-js";
import { api } from "../../services/api";
import { AltBadge, shortenURL } from "./imageHelpers";

/**
 * One inline-image card. Resolves its src as: the served (object
 * URL) when the row has been mirrored to storage, otherwise the
 * remote `url`. The blob URL is cached for the card's lifetime
 * and revoked on cleanup to avoid leaking blob memory.
 *
 * Props:
 *   - image: the source_images row
 *   - slug, sourceID: route params for the serving endpoint
 */
export default function InlineCard(props) {
  const stored = () => !!props.image.storage_key;
  const [blobUrl, setBlobUrl] = createSignal(null);
  const [failed, setFailed] = createSignal(false);

  const src = createMemo(() => {
    if (!stored() || failed()) return props.image.url;
    if (blobUrl()) return blobUrl();
    api
      .getSourceImage(props.slug, props.sourceID, props.image.id)
      .then((url) => {
        if (url) setBlobUrl(url);
        else setFailed(true);
      })
      .catch(() => setFailed(true));
    return props.image.url;
  });

  onCleanup(() => {
    if (blobUrl()) URL.revokeObjectURL(blobUrl());
  });

  const href = () => blobUrl() || props.image.url;

  return (
    <a
      href={href()}
      target="_blank"
      rel="noopener noreferrer"
      class="block border rounded dark:border-gray-700 bg-gray-50 dark:bg-gray-900 overflow-hidden hover:border-blue-400 dark:hover:border-blue-500 transition"
      title={props.image.alt_text ? `Alt: ${props.image.alt_text}` : "No alt text"}
    >
      <img
        src={src()}
        alt={props.image.alt_text || ""}
        loading="lazy"
        class="w-full h-32 object-cover bg-white dark:bg-gray-800"
      />
      <div class="p-1 space-y-1">
        <AltBadge alt={props.image.alt_text} />
        <p class="text-[10px] font-mono truncate text-gray-500 dark:text-gray-400">
          {shortenURL(props.image.url)}
        </p>
      </div>
    </a>
  );
}
