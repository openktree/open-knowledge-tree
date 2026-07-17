import { Show } from "solid-js";
import Card from "../../components/Card";
import Button from "../../components/Button";
import FactBadges from "../../components/FactBadges";
import FactConceptTags from "../../components/FactConceptTags";
import ImageFromUrl from "../../components/ImageFromUrl";
import FactSourceList from "./FactSourceList";

// FactDetailContent renders the fact text, kind + status + source
// -count badges, optional image (image facts), metadata block
// (created_at, image URL, embedded model), and the full source
// list. The source list is the human-in-the-loop validation
// surface: each source links to the SourceDetail page and to the
// external URL so the user can open the source and confirm the
// fact was actually extracted from it.
const fmt = (ts) => (ts ? new Date(ts).toLocaleString() : "");

export default function FactDetailContent(props) {
  const fact = () => props.fact() || {};
  const isImage = () => fact()?.fact_kind === "image" && !!fact()?.image_url;

  return (
    <Card>
      <div class="flex items-center justify-between mb-3">
        <h2 class="text-lg font-semibold dark:text-white">Fact detail</h2>
        <Button variant="secondary" onClick={props.onRefresh} class="text-xs px-2 py-1">
          Refresh
        </Button>
      </div>
      <div class="border rounded dark:border-gray-700 p-4 mb-4 text-sm text-gray-700 dark:text-gray-300 space-y-3">
        <Show when={isImage()}>
          <ImageFromUrl
            imageUrl={fact().image_url}
            alt=""
            class="max-h-80 rounded border dark:border-gray-600 hover:opacity-90"
          />
        </Show>
        <div class="text-base">{fact()?.text}</div>
        <FactBadges fact={fact()} slug={props.slug} />
      </div>

      <FactConceptTags slug={props.slug} factID={fact()?.id} concepts={props.concepts} />

      <div class="border rounded dark:border-gray-700 p-4 mb-4 text-xs text-gray-600 dark:text-gray-400 space-y-1">
        <div class="font-semibold text-gray-700 dark:text-gray-300 mb-1">Metadata</div>
        <div>Kind: {fact()?.fact_kind || "text"}</div>
        <Show when={fact()?.image_url}>
          <div class="break-all">Image URL: <a href={fact().image_url} target="_blank" rel="noopener noreferrer" class="text-blue-600 hover:underline dark:text-blue-400">{fact().image_url}</a></div>
        </Show>
        <div>Status: {fact()?.status || ""}</div>
        <Show when={fact()?.created_at}>
          <div>Created: {fmt(fact()?.created_at)}</div>
        </Show>
        <Show when={fact()?.embedded_model}>
          <div>Embedded model: {fact()?.embedded_model}</div>
        </Show>
        <Show when={fact()?.embedded_at}>
          <div>Embedded at: {fmt(fact()?.embedded_at)}</div>
        </Show>
      </div>

      <Show
        when={(props.sources() || []).length > 0}
        fallback={
          <p class="text-sm text-gray-500 dark:text-gray-400">
            No sources linked to this fact.
          </p>
        }
      >
        <FactSourceList sources={props.sources} slug={props.slug} />
      </Show>
    </Card>
  );
}