import { A } from "@solidjs/router";
import { Show } from "solid-js";
import Badge from "../../components/Badge";
import {
  oaStatusCopy,
  oaStatusVariant,
  parseStatusCopy,
  parseStatusVariant,
  UNTITLED_FALLBACK,
} from "./constants";

/**
 * Header strip for the SourceDetail page. Renders the
 * parsed title (or a fallback), the URL, and a row of
 * metadata pills (sitename, author, parse status,
 * language). The original raw body is intentionally
 * not shown here or anywhere else on the page.
 *
 * Props:
 *   - source:  accessor returning the source row
 *   - slug:    string, used to build the "back" link
 *   - error:   string | null, the error from the row
 */
export default function SourceHeader(props) {
  const source = () => props.source();
  const title = () => {
    const t = source()?.parsed_title;
    return t && t.trim().length > 0 ? t : UNTITLED_FALLBACK;
  };
  const status = () => {
    const s = source();
    if (!s) return "pending";
    return s.parse_status || "pending";
  };
  return (
    <header class="space-y-3">
      <div class="flex items-center gap-2 text-xs text-gray-500 dark:text-gray-400">
        <A href={`/${props.slug}/sources`} class="text-blue-600 dark:text-blue-400 hover:underline">
          ← All sources
        </A>
        <span>·</span>
        <span class="font-mono truncate" title={source()?.url}>
          {source()?.url}
        </span>
      </div>

      <h1 class="text-2xl font-semibold text-gray-900 dark:text-white leading-tight">{title()}</h1>

      <div class="flex items-center gap-2 flex-wrap">
        <Badge variant={parseStatusVariant[status()] || "gray"}>
          {parseStatusCopy[status()] || status()}
        </Badge>
        <Show when={source()?.parsed_sitename}>
          <Badge variant="purple">{source().parsed_sitename}</Badge>
        </Show>
        <Show when={source()?.parsed_author}>
          <Badge variant="blue">by {source().parsed_author}</Badge>
        </Show>
        <Show when={source()?.parsed_language}>
          <Badge variant="gray">{source().parsed_language}</Badge>
        </Show>
        <Show when={source()?.doi}>
          <Badge variant="gray">DOI {source().doi}</Badge>
        </Show>
        <Show when={source()?.oa_status}>
          <Badge
            variant={oaStatusVariant[source().oa_status] || "gray"}
            title={
              source().oa_status === "closed"
                ? "This article is paywalled. Only the publicly visible portion (abstract/references) was retrieved."
                : undefined
            }
          >
            {oaStatusCopy[source().oa_status] || source().oa_status}
          </Badge>
        </Show>
        <Show when={source()?.kind}>
          <Badge variant="gray">{source().kind}</Badge>
        </Show>
      </div>

      <Show when={props.error}>
        <p class="text-sm text-red-600 dark:text-red-400" title={props.error}>
          {props.error}
        </p>
      </Show>
    </header>
  );
}
