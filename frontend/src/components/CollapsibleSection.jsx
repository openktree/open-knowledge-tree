import { Show, createSignal } from "solid-js";

// CollapsibleSection wraps a block of content behind a clickable
// header that toggles visibility. The header is always rendered
// (so the user can re-expand); only the body hides. Used by the
// Investigation Sources phase to let the user collapse the search
// panel, the existing-source picker, and the source list
// independently so they can move between search and review without
// scrolling past collapsed sections.
//
// Props:
//   - title:    string              — heading text
//   - subtitle: string (optional)   — muted line under the title
//   - defaultOpen: boolean (default true)
//   - headerRight: JSX (optional)   — extra elements rendered on
//                                     the right of the header row
//                                     (e.g. search/refresh). The
//                                     headerRight is always visible
//                                     regardless of open state.
//   - children: JSX                  — the collapsible body
export default function CollapsibleSection(props) {
  const [open, setOpen] = createSignal(props.defaultOpen ?? true);
  const toggle = () => {
    const next = !open();
    setOpen(next);
    props.onToggle?.(next);
  };

  return (
    <div class={`rounded-lg shadow-card dark:shadow-card-dark p-6 ${props.class || ""} bg-surface border border-border`}>
      <div class="flex items-center justify-between gap-3 flex-wrap">
        <button
          type="button"
          class="text-left min-w-0 flex-1"
          onClick={toggle}
          aria-expanded={open()}
        >
          <div class="flex items-center gap-2">
            <span class="text-text-muted text-sm w-4 inline-block">
              {open() ? "\u25bc" : "\u25b6"}
            </span>
            <h2 class="text-lg font-semibold text-text-base">{props.title}</h2>
          </div>
          <Show when={props.subtitle}>
            <p class="text-sm text-text-muted mt-1 ml-6">
              {props.subtitle}
            </p>
          </Show>
        </button>
        <Show when={props.headerRight}>
          <div class="flex items-center gap-2">{props.headerRight}</div>
        </Show>
      </div>
      <Show when={open()}>
        <div class="mt-4">{props.children}</div>
      </Show>
    </div>
  );
}