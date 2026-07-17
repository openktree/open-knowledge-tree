import { Show } from "solid-js";
import { A } from "@solidjs/router";
import EmptyState from "../../components/EmptyState";
import Button from "../../components/Button";

/**
 * Network / server-error fallback for the SourceDetail
 * page. The data resource's error field is an Error
 * thrown by api.getSource; map common shapes (404, 403,
 * generic) onto UI copy.
 *
 * Kept as a separate file (rather than an internal
 * subcomponent of the page index) so the page index
 * stays under the 100-line internal-subcomponents
 * limit.
 *
 * Props:
 *   - error:  Error | null
 *   - onRetry: () => void
 *   - slug:   string
 */
export default function FetchErrorState(props) {
  const code = () => {
    const msg = props.error?.message || "";
    if (msg.includes("not found")) return 404;
    if (msg.includes("permission") || msg.includes("unauthorized") || msg.includes("forbidden")) return 403;
    return 0;
  };
  return (
    <div class="space-y-4">
      <EmptyState
        title={
          code() === 404
            ? "Source not found."
            : code() === 403
              ? "You do not have permission to view this source."
              : "Could not load this source."
        }
        description={props.error?.message || "An unexpected error occurred."}
      />
      <div class="flex items-center gap-2">
        <Button variant="secondary" onClick={props.onRetry}>
          Retry
        </Button>
        <A
          href={`/${props.slug}/sources`}
          class="text-sm text-blue-600 dark:text-blue-400 hover:underline"
        >
          Back to all sources
        </A>
      </div>
    </div>
  );
}
