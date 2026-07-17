import { Show } from "solid-js";
import { A, useParams } from "@solidjs/router";
import Badge from "./Badge";

// Status badge color map. `new` is amber (in flight through the
// embed+dedup pipeline), `stable` is green (confirmed unique),
// `to_delete` is red with a strikethrough on the text (loser of a
// dedup merge, pending cleanup).
const STATUS_VARIANT = {
  new: "yellow",
  stable: "green",
  to_delete: "red",
};

// FactBadges renders the status badge and the source-count badge
// for a fact. The source-count badge is a link to the fact detail
// page (`/:slug/facts/:factID`) so the user can validate the fact
// against every source that supports it. The slug comes from the
// route params; when absent (e.g. the badge is rendered outside a
// /:slug route) the link degrades to a non-clickable span.
// Kind badge label/variant. `image` facts carry a non-null
// image_url and render a thumbnail in the row/detail; `text` is
// the default kind. Any unknown kind falls back to gray.
const KIND_VARIANT = {
  image: "purple",
  text: "gray",
};

export default function FactBadges(props) {
  const params = useParams();
  const slug = () => props.slug || params.slug;
  const status = () => props.fact?.status;
  const sourceCount = () => Number(props.fact?.source_count || 0);
  const factID = () => props.fact?.id;
  const kind = () => props.fact?.fact_kind || "text";

  return (
    <div class="flex items-center gap-2 flex-wrap">
      <Badge variant={KIND_VARIANT[kind()] || "gray"}>
        {kind() === "image" ? "Image" : "Text"}
      </Badge>
      <Badge variant={STATUS_VARIANT[status()] || "gray"}>
        <span class={status() === "to_delete" ? "line-through" : ""}>
          {status()}
        </span>
      </Badge>
      <Show
        when={slug() && factID()}
        fallback={
          <Badge variant="blue">
            {sourceCount() === 1 ? "Mentioned by" : "Confirmed by"} {sourceCount()} {sourceCount() === 1 ? "source" : "sources"}
          </Badge>
        }
      >
        <A
          href={`/${slug()}/facts/${factID()}`}
          class="text-xs px-2 py-0.5 rounded font-mono bg-primary/10 text-primary-fg hover:bg-primary/20"
          title="View sources for this fact"
        >
          {sourceCount() === 1 ? "Mentioned by" : "Confirmed by"} {sourceCount()} {sourceCount() === 1 ? "source" : "sources"}
        </A>
      </Show>
      {props.extra}
    </div>
  );
}