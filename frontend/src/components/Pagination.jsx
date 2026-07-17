import { createMemo, For, Show } from "solid-js";
import Button from "./Button";

// Pagination renders prev/next + page-number buttons using the
// server's total + limit + offset. Emits onOffsetChange with the
// new absolute offset (clamped to [0, maxOffset]). The parent
// owns the offset signal; this component is purely controlled.
//
// The total comes from the server's pageEnvelope so the page
// count is globally consistent (an in-memory count would drift
// when filters narrow the set). When total is 0 the bar hides
// itself so an empty result doesn't show "page 1 of 0".
//
// `total` and `limit` are accepted as either an accessor
// (() => number) or a raw number; some callers pass one form,
// some the other, and the component should not have to care.
const PAGE_WINDOW = 2;
const MAX_DIRECT_PAGES = 7;

export default function Pagination(props) {
  const total = () => {
    const v = typeof props.total === "function" ? props.total() : props.total;
    return Number(v || 0);
  };
  const limit = () => {
    const v = typeof props.limit === "function" ? props.limit() : props.limit;
    return Number(v || 100);
  };
  const offset = () => Number(props.offset || 0);

  const currentPage = () => (total() <= 0 ? 0 : Math.floor(offset() / limit()) + 1);
  const totalPages = () => (total() <= 0 ? 0 : Math.max(1, Math.ceil(total() / limit())));
  const maxOffset = () => Math.max(0, (totalPages() - 1) * limit());

  const prev = () => {
    const next = Math.max(0, offset() - limit());
    if (next !== offset()) props.onOffsetChange?.(next);
  };
  const next = () => {
    const nx = offset() + limit();
    if (nx <= maxOffset()) props.onOffsetChange?.(nx);
  };
  const goto = (page) => {
    if (page < 1 || page > totalPages()) return;
    props.onOffsetChange?.((page - 1) * limit());
  };

  // Build the sequence of page buttons + "…" ellipsis sentinels.
  // - When totalPages <= MAX_DIRECT_PAGES, render every page.
  // - Otherwise, always show 1 and N, plus a window of ±PAGE_WINDOW
  //   around the current page, with "…" wherever the gap is > 1.
  const pageNumbers = createMemo(() => {
    const tp = totalPages();
    const cp = currentPage();
    if (tp === 0) return [];
    if (tp <= MAX_DIRECT_PAGES) {
      return Array.from({ length: tp }, (_, i) => i + 1);
    }
    const start = Math.max(2, cp - PAGE_WINDOW);
    const end = Math.min(tp - 1, cp + PAGE_WINDOW);
    const out = [1];
    if (start > 2) out.push("…");
    for (let p = start; p <= end; p++) out.push(p);
    if (end < tp - 1) out.push("…");
    out.push(tp);
    return out;
  });

  return (
    <Show when={total() > 0}>
      <nav
        role="navigation"
        aria-label="Pagination"
        class="flex items-center justify-between gap-3 mt-4 text-sm text-text-muted"
      >
        <span>
          Page {currentPage()} of {totalPages()} ({total().toLocaleString()} total)
        </span>
        <div class="flex items-center gap-1 flex-wrap">
          <Button
            variant="secondary"
            onClick={prev}
            disabled={offset() === 0}
            class="text-xs px-2 py-1"
            aria-label="Previous page"
          >
            Prev
          </Button>
          <For each={pageNumbers()}>
            {(p) =>
              p === "…" ? (
                <span class="px-2 text-text-muted select-none" aria-hidden="true">
                  …
                </span>
              ) : (
                <Button
                  variant={p === currentPage() ? "active" : "secondary"}
                  onClick={() => goto(p)}
                  aria-current={p === currentPage() ? "page" : undefined}
                  aria-label={`Go to page ${p}`}
                  class="text-xs px-2 py-1 min-w-[2rem]"
                >
                  {p}
                </Button>
              )
            }
          </For>
          <Button
            variant="secondary"
            onClick={next}
            disabled={offset() + limit() >= total()}
            class="text-xs px-2 py-1"
            aria-label="Next page"
          >
            Next
          </Button>
        </div>
      </nav>
    </Show>
  );
}
