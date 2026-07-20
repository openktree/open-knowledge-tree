import { createSignal, onCleanup, Show } from "solid-js";
import { api } from "../services/api";

// resolveStorageImageURL returns the path-portion of the
// service-routable image URL the worker synthesizes for page
// renders (kind='page' with no remote URL). Returns null when
// the URL is not same-origin storage. The worker emits paths
// like `/api/v1/repositories/{slug}/sources/{sourceID}/images/{imageID}`
// or (via the Vite dev proxy) `/repositories/{slug}/sources/...`
// — both prefixes are matched here so dev and prod both work.
//
// We also extract {slug, sourceID, imageID} so the blob fetch can
// reuse api.getSourceImage instead of re-implementing the auth
// header logic. The blob URL is rendered by the caller via
// createMemo + onCleanup (see ImageFromUrl below).
const STORAGE_PATH_RE =
  /^\/(?:api\/v1\/)?repositories\/([^/]+)\/sources\/([^/]+)\/images\/([^/]+)$/;

export function resolveStorageImageURL(imageUrl) {
  if (!imageUrl || typeof imageUrl !== "string") return null;
  const m = imageUrl.match(STORAGE_PATH_RE);
  if (!m) return null;
  return { slug: m[1], sourceID: m[2], imageID: m[3] };
}

// ImageFromUrl renders an <img> from a fact's image_url. When
// the URL points at our own storage (a service-routable URL the
// decomposition worker synthesizes for PDF page renders), the
// bytes are fetched through the authenticated getSourceImage
// helper (a plain <img src=...> would fail because browsers do
// not send the Authorization header on <img> requests). When
// the URL is external (an inline image's remote URL), it is
// passed straight to <img>. On 404 from the storage endpoint
// the fallback is the original URL so a not-yet-mirrored row
// degrades gracefully.
//
// The blob URL is created lazily on first paint (via the ref
// callback) and revoked on cleanup to avoid leaking blob memory.
//
// Props:
//   - imageUrl: string (required)
//   - alt:      string (optional)
//   - class:    string (optional, applied to <img>)
//   - loading:  "lazy" | "eager" (optional, default "lazy")
//   - href:     string (optional override; otherwise the original
//               imageUrl is used as the link target for external URLs
//               and the blob URL once it's available)
export default function ImageFromUrl(props) {
  const storage = () => resolveStorageImageURL(props.imageUrl);
  const isStorage = () => !!storage();

  const [blobUrl, setBlobUrl] = createSignal(null);
  const [failed, setFailed] = createSignal(false);

  const fetchBlob = () => {
    const s = storage();
    if (!s || blobUrl() || failed()) return;
    api
      .getSourceImage(s.slug, s.sourceID, s.imageID)
      .then((url) => {
        if (url) setBlobUrl(url);
        else setFailed(true);
      })
      .catch(() => setFailed(true));
  };

  onCleanup(() => {
    if (blobUrl()) URL.revokeObjectURL(blobUrl());
  });

  const src = () => {
    if (!isStorage()) return props.imageUrl;
    if (failed()) return props.imageUrl;
    if (blobUrl()) return blobUrl();
    return null;
  };

  const href = () => {
    if (!isStorage()) return props.imageUrl;
    return blobUrl() || props.imageUrl;
  };

  return (
    <Show when={src() || isStorage()} fallback={null}>
      <Show
        when={isStorage() && !blobUrl() && !failed()}
        fallback={
          <a href={href()} target="_blank" rel="noopener noreferrer">
            <img
              src={src()}
              alt={props.alt || ""}
              loading={props.loading || "lazy"}
              class={props.class}
            />
          </a>
        }
      >
        <div
          class={props.class}
          style={{ display: "flex", "align-items": "center", "justify-content": "center" }}
          ref={(el) => {
            if (el) fetchBlob();
          }}
        />
      </Show>
    </Show>
  );
}
