import { Show, For } from "solid-js";
import { A } from "@solidjs/router";
import Modal from "./Modal";
import FactBadges from "./FactBadges";
import { postureStyle } from "../pages/ReportDetail/constants";

export default function CitedFactModal(props) {
  const count = () => (props.facts || []).length;
  const title = () =>
    props.sentenceIndex == null
      ? "Facts"
      : `Facts from sentence #${props.sentenceIndex + 1}`;

  return (
    <Modal open={props.open} onClose={props.onClose} title={title()}>
      <Show
        when={count() > 0}
        fallback={
          <p class="text-text-muted italic">
            No facts matched this sentence.
          </p>
        }
      >
        <div class="space-y-3">
          <p class="text-xs text-text-muted">
            {count()} {count() === 1 ? "fact" : "facts"} matched:
          </p>
          <For each={props.facts || []}>
            {(f) => (
              <A
                href={`/${props.slug}/facts/${f.fact_id}`}
                class="block p-3 border border-border rounded hover:bg-primary-soft transition-colors"
                onClick={() => props.onClose?.()}
              >
                <div class="space-y-2">
                  <div class="flex items-start justify-between gap-2">
                    <div class="text-text-base">{f.text}</div>
                    <div class="shrink-0 flex items-center gap-1.5">
                      <Show when={postureStyle[f.posture]}>
                        {(p) => (
                          <span class={`text-xs font-medium px-1.5 py-0.5 rounded ${p().class}`}>
                            {p().label}
                          </span>
                        )}
                      </Show>
                      <Show when={f.score != null}>
                        <span class="text-xs font-mono font-semibold text-primary-fg">
                          {(f.score * 100).toFixed(1)}%
                        </span>
                      </Show>
                    </div>
                  </div>
                  <FactBadges
                    fact={{
                      id: f.fact_id,
                      status: f.status,
                      fact_kind: f.fact_kind,
                      image_url: f.image_url,
                      source_count: f.source_count || 0,
                    }}
                    slug={props.slug}
                  />
                </div>
              </A>
            )}
          </For>
        </div>
      </Show>
    </Modal>
  );
}
