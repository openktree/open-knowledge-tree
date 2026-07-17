import { Show } from "solid-js";
import { A } from "@solidjs/router";
import EmptyState from "../../components/EmptyState";
import Button from "../../components/Button";

/**
 * Inverts the typical "wrap the children" pattern: by
 * default renders the children, but when the source has
 * no parsed content yet (parse_status is NULL or
 * 'pending') replaces them with a waiting-for-parse
 * empty state. The user can come back and refresh; the
 * URL is shareable so the same wait works for a
 * teammate.
 *
 * Lives in its own file so the SourceDetail page index
 * stays under the 100-line internal-subcomponents
 * limit. Both states below are pure presentation and
 * take the same shape: a small empty state plus a
 * retry/back control.
 *
 * Props:
 *   - source:  the source row
 *   - copy:    the parseStatusCopy map (so the caller
 *              controls the dictionary; keeps this
 *              file free of detail-page knowledge)
 *   - slug:    string, for the "back" link
 *   - onRetry: () => void
 */
export default function ParseEmptyState(props) {
  const status = () => {
    const s = props.source;
    if (!s) return "pending";
    return s.parse_status || "pending";
  };
  const showChildren = () => {
    const s = status();
    return s === "ok" || s === "unsupported";
  };
  return (
    <Show
      when={!showChildren()}
      fallback={props.children}
    >
      <div class="space-y-4">
        <EmptyState
          title={props.copy[status()] || "Awaiting parse"}
          description="The Retrieve Source worker hasn't finished extracting content from this URL yet. Refresh to check again, or come back later — this URL is shareable."
        />
        <div class="flex items-center gap-2">
          <Button variant="secondary" onClick={props.onRetry}>
            Refresh
          </Button>
          <A
            href={`/${props.slug}/sources`}
            class="text-sm text-blue-600 dark:text-blue-400 hover:underline"
          >
            Back to all sources
          </A>
        </div>
      </div>
    </Show>
  );
}
