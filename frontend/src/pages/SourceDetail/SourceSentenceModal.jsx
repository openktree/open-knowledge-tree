import { Show, For } from "solid-js";
import { A } from "@solidjs/router";
import Modal from "../../components/Modal";
import FactBadges from "../../components/FactBadges";

/**
 * Centered modal listing the facts derived from a single clicked
 * sentence. Receives the resolved facts array (the
 * ListFactReferencesBySource rows whose sentence_index matches the
 * clicked sentence) and renders one card per fact with status/kind
 * badges and a link to the fact detail page.
 *
 * Props:
 *   - open:    boolean
 *   - onClose: () => void
 *   - sentenceIndex: number | null
 *   - facts:   array of { fact_id, text, status, fact_kind, image_url }
 *   - slug:    string for the fact detail link
 */
export default function SourceSentenceModal(props) {
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
          <p class="text-gray-500 dark:text-gray-400 italic">
            No facts were extracted from this sentence.
          </p>
        }
      >
        <div class="space-y-3">
          <p class="text-xs text-gray-500 dark:text-gray-400">
            {count()} {count() === 1 ? "fact" : "facts"} derived from this sentence:
          </p>
          <For each={props.facts || []}>
            {(ref) => (
              <A
                href={`/${props.slug}/facts/${ref.fact_id}`}
                class="block p-3 border rounded dark:border-gray-700 hover:bg-gray-50 dark:hover:bg-gray-800/50 transition-colors"
                onClick={() => props.onClose?.()}
              >
                <div class="space-y-2">
                  <div class="text-gray-800 dark:text-gray-200">{ref.text}</div>
                  <FactBadges
                    fact={{
                      id: ref.fact_id,
                      status: ref.status,
                      fact_kind: ref.fact_kind,
                      image_url: ref.image_url,
                      source_count: 0,
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