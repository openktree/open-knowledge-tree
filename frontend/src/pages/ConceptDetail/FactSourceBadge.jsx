import { createResource, For, Show } from "solid-js";
import { api } from "../../services/api";

const MAX_VISIBLE = 3;

// FactSourceBadge fetches the fact's sources asynchronously (via
// getFact) and renders up to MAX_VISIBLE source hostnames, each
// linking to its external URL. When there are more sources than
// shown, a "+N more" element displays the full list on hover
// (title attribute). Used in compact fact lists where the list
// endpoint doesn't return source URLs.
//
// Props:
//   - slug: repo slug
//   - factID: fact UUID
export default function FactSourceBadge(props) {
  const [data] = createResource(
    () => ({ slug: props.slug, factID: props.factID }),
    async ({ slug, factID }) => {
      if (!slug || !factID) return null;
      try {
        return await api.getFact(slug, factID);
      } catch {
        return null;
      }
    }
  );

  const sources = () => data()?.sources || [];

  const hostOf = (url) => {
    if (!url) return null;
    try {
      return new URL(url).hostname;
    } catch {
      return url;
    }
  };

  const visible = () => sources().slice(0, MAX_VISIBLE);
  const extra = () => sources().slice(MAX_VISIBLE);
  const allUrlsTitle = () => sources().map((s) => s.url).filter(Boolean).join("\n");

  return (
    <Show when={!data.loading && sources().length > 0}>
      <div class="flex items-center gap-1.5 flex-wrap">
        <For each={visible()}>
          {(src) => {
            const h = hostOf(src.url);
            return (
              <Show when={h}>
                <a
                  href={src.url}
                  target="_blank"
                  rel="noopener noreferrer"
                  class="text-xs px-1.5 py-0.5 rounded bg-gray-100 dark:bg-gray-700 text-gray-500 dark:text-gray-400 hover:text-gray-700 dark:hover:text-gray-200 transition truncate max-w-[120px]"
                  title={src.url}
                  onClick={(e) => e.stopPropagation()}
                >
                  {h}
                </a>
              </Show>
            );
          }}
        </For>
        <Show when={extra().length > 0}>
          <span
            class="text-xs px-1.5 py-0.5 rounded bg-gray-100 dark:bg-gray-700 text-gray-400 dark:text-gray-500 cursor-help"
            title={extra().map((s) => s.url).filter(Boolean).join("\n")}
          >
            +{extra().length} more
          </span>
        </Show>
      </div>
    </Show>
  );
}