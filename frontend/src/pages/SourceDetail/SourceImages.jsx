import { createSignal, Show } from "solid-js";
import InlineGallery from "./InlineGallery";
import PageGallery from "./PageGallery";

/**
 * Image gallery for the SourceDetail page. Splits the image list
 * returned by the backend into two sub-galleries: inline <img>
 * URLs (extracted from HTML) and PDF page renders (one row per
 * page). Each sub-gallery handles its own blob-URL fetching from
 * the authenticated serving endpoint.
 *
 * Props:
 *   - images: accessor returning the image list
 *     (OktRepositorySourceImage rows)
 *   - slug:   string (the route's :slug)
 *   - sourceID: string (the route's :sourceID)
 */
export default function SourceImages(props) {
  const inlineImages = () => (props.images() || []).filter((i) => i.kind === "inline");
  const pageImages = () => (props.images() || []).filter((i) => i.kind === "page");
  const [showAll, setShowAll] = createSignal(false);

  return (
    <Show when={(props.images() || []).length > 0}>
      <section class="space-y-6">
        <Show when={inlineImages().length > 0}>
          <InlineGallery
            images={inlineImages}
            showAll={showAll}
            setShowAll={setShowAll}
            slug={props.slug}
            sourceID={props.sourceID}
          />
        </Show>
        <Show when={pageImages().length > 0}>
          <PageGallery images={pageImages} slug={props.slug} sourceID={props.sourceID} />
        </Show>
      </section>
    </Show>
  );
}