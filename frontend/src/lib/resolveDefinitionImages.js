import { resolveStorageImageURL } from "../components/ImageFromUrl";
import { api } from "../services/api";

// resolveDefinitionImages resolves each embedded image fact's
// image_url to a renderable URL for an <img> tag.
//
// The synthesis (definition) emits ![alt](<fact:fact_id>) citations; the
// DefinitionPanel rewrites them to ![alt](renderableUrl) before
// micromark renders the body. Storage image_urls (paths like
// /api/v1/repositories/{slug}/sources/{src}/images/{img}) require
// auth headers a plain <img> can't send, so they are fetched as
// blobs via api.getSourceImage and exposed as object URLs. External
// http(s) URLs pass through unchanged.
//
// Returns { map, blobUrls } where map is fact_id -> renderableUrl
// and blobUrls is the list of object URLs the caller must revoke on
// cleanup / re-resolve. A storage fetch failure falls back to the
// raw URL so the image degrades gracefully instead of blanking.
export async function resolveDefinitionImages(images) {
  const map = new Map();
  const blobUrls = [];
  await Promise.all(
    images.map(async (img) => {
      if (!img.image_url) return;
      const storage = resolveStorageImageURL(img.image_url);
      if (storage) {
        try {
          const blobUrl = await api.getSourceImage(storage.slug, storage.sourceID, storage.imageID);
          if (blobUrl) {
            map.set(img.id, blobUrl);
            blobUrls.push(blobUrl);
            return;
          }
        } catch {
          // fall through to raw URL
        }
      }
      map.set(img.id, img.image_url);
    }),
  );
  return { map, blobUrls };
}

// revokeBlobUrls revokes a list of object URLs, ignoring errors.
export function revokeBlobUrls(urls) {
  for (const u of urls) {
    try {
      URL.revokeObjectURL(u);
    } catch {
      /* noop */
    }
  }
}
