import { createEffect, onCleanup, Show } from "solid-js";
import { Portal } from "solid-js/web";

/**
 * Centered modal dialog rendered via Portal so it escapes parent
 * overflow / z-index contexts. Closes on Escape, backdrop click, or
 * the onClose callback. No outside dep — uses Solid's built-in
 * Portal.
 *
 * Props:
 *   - open:    boolean accessor/signal
 *   - onClose: () => void
 *   - title:   string | null  (optional header)
 *   - children: SolidJS children
 */
export default function Modal(props) {
  const onKey = (e) => {
    if (e.key === "Escape") props.onClose?.();
  };
  createEffect(() => {
    if (props.open) {
      document.addEventListener("keydown", onKey);
      document.body.style.overflow = "hidden";
    } else {
      document.removeEventListener("keydown", onKey);
      document.body.style.overflow = "";
    }
  });
  onCleanup(() => {
    document.removeEventListener("keydown", onKey);
    document.body.style.overflow = "";
  });

  return (
    <Show when={props.open}>
      <Portal>
        <div
          class="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4"
          onClick={() => props.onClose?.()}
        >
          <div
            class="bg-surface border border-border rounded-lg shadow-xl max-w-lg w-full max-h-[80vh] flex flex-col"
            onClick={(e) => e.stopPropagation()}
          >
            <Show when={props.title}>
              <div class="px-5 py-3 border-b border-border flex items-center justify-between">
                <h3 class="text-sm font-semibold text-text-base">{props.title}</h3>
                <button
                  class="text-text-muted hover:text-text-base text-lg leading-none"
                  onClick={() => props.onClose?.()}
                  aria-label="Close"
                >
                  ×
                </button>
              </div>
            </Show>
            <div class="px-5 py-4 overflow-y-auto text-sm text-text-muted">{props.children}</div>
          </div>
        </div>
      </Portal>
    </Show>
  );
}
