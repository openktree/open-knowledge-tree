import { Show } from "solid-js";
import { A, useParams } from "@solidjs/router";
import FactBadges from "../../components/FactBadges";
import ImageFromUrl from "../../components/ImageFromUrl";

// FactRow renders a single fact in the repo-level Facts list. The
// whole row is a link to the fact detail page
// (`/:slug/facts/:factID`) so any fact — text or image — is
// clickable and shareable. Image facts (fact_kind === 'image' &&
// image_url) render a thumbnail alongside the text.
//
// `slug` is required for the row to be a link; when absent the
// row degrades to a non-clickable card and FactBadges falls
// back to useParams(). The source-count badge inside FactBadges
// still links independently to the same detail page.
export default function FactRow(props) {
  const params = useParams();
  const slug = () => props.slug || params.slug;
  const factID = () => props.fact?.id;
  const isImage = () => props.fact?.fact_kind === "image" && !!props.fact?.image_url;
  const detailHref = () => `/${slug()}/facts/${factID()}`;
  const hasLink = () => !!slug() && !!factID();

  return (
    <div class="border rounded dark:border-gray-700 text-sm text-gray-700 dark:text-gray-300 overflow-hidden">
      <Show
        when={hasLink()}
        fallback={
          <div class="p-3 space-y-2">
            <div class="flex items-start gap-3">
              <Show when={isImage()}>
                <ImageFromUrl
                  imageUrl={props.fact.image_url}
                  class="h-16 w-16 object-cover rounded border dark:border-gray-600"
                />
              </Show>
              <div class="min-w-0 flex-1">
                <div>{props.fact.text}</div>
              </div>
            </div>
            <FactBadges fact={props.fact} slug={props.slug} extra={props.extra} />
          </div>
        }
      >
        <A
          href={detailHref()}
          class="block p-3 space-y-2 hover:bg-gray-50 dark:hover:bg-gray-800/50 transition-colors"
          title="View fact detail"
        >
          <div class="flex items-start gap-3">
            <Show when={isImage()}>
              <ImageFromUrl
                imageUrl={props.fact.image_url}
                class="h-16 w-16 object-cover rounded border dark:border-gray-600"
              />
            </Show>
            <div class="min-w-0 flex-1">
              <div>{props.fact.text}</div>
            </div>
          </div>
          <FactBadges fact={props.fact} slug={props.slug} extra={props.extra} />
        </A>
      </Show>
    </div>
  );
}