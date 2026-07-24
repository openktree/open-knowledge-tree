import { A } from "@solidjs/router";
import { createResource, createSignal, For, Show } from "solid-js";
import { postureStyle } from "../pages/ReportDetail/constants";
import { api } from "../services/api";
import FactBadges from "./FactBadges";
import Modal from "./Modal";

// CitedFactModal shows the facts matched against a clicked sentence in
// the report annotation view. Each fact row shows the fact text,
// posture badge, score, kind/status badges, and the source URLs
// (fetched on open via parallel api.getFact calls so the reader can
// verify the fact against its sources without leaving the modal).
export default function CitedFactModal(props) {
  const count = () => (props.facts || []).length;
  const title = () =>
    props.sentenceIndex == null ? "Facts" : `Facts from sentence #${props.sentenceIndex + 1}`;

  const sentenceText = () => (props.facts || [])[0]?.sentence_text;

  // Fetch source URLs for each fact when the modal opens. The
  // annotation rows carry fact_id but not source URLs, so we fetch
  // them in parallel. A 404 (fact deleted) is caught per-fact so the
  // modal still shows the other facts.
  const [factSources] = createResource(
    () => (props.open && count() > 0 ? props.facts.map((f) => f.fact_id) : null),
    async (factIds) => {
      const map = new Map();
      await Promise.all(
        factIds.map(async (fid) => {
          try {
            const res = await api.getFact(props.slug, fid);
            map.set(fid, res.sources || []);
          } catch {
            map.set(fid, []);
          }
        }),
      );
      return map;
    },
  );

  const sourcesFor = (fid) => factSources()?.get(fid) || [];

  return (
    <Modal open={props.open} onClose={props.onClose} title={title()}>
      <Show
        when={count() > 0}
        fallback={<p class="text-text-muted italic">No facts matched this sentence.</p>}
      >
        <div class="space-y-3">
          <Show when={sentenceText()}>
            <blockquote class="border-l-2 border-border pl-3 py-1 text-text-base italic">
              {sentenceText()}
            </blockquote>
          </Show>
          <p class="text-xs text-text-muted">
            {count()} {count() === 1 ? "fact" : "facts"} matched:
          </p>
          <For each={props.facts || []}>
            {(f) => (
              <div class="p-3 border border-border rounded space-y-2">
                <A
                  href={`/${props.slug}/facts/${f.fact_id}`}
                  class="block hover:bg-primary-soft transition-colors -m-3 p-3 rounded"
                  onClick={() => props.onClose?.()}
                >
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
                </A>
                {/* Source URLs (fetched on open) */}
                <Show when={sourcesFor(f.fact_id).length > 0}>
                  <div class="pt-1 border-t border-border">
                    <div class="text-xs text-text-muted mb-1">Sources:</div>
                    <ul class="space-y-0.5">
                      <For each={sourcesFor(f.fact_id)}>
                        {(src) => (
                          <li>
                            <a
                              href={src.url}
                              target="_blank"
                              rel="noopener noreferrer"
                              class="text-xs text-primary-fg hover:underline break-all"
                            >
                              {src.parsed_title || src.url}
                            </a>
                            <Show when={src.parsed_sitename}>
                              <span class="text-xs text-text-muted"> — {src.parsed_sitename}</span>
                            </Show>
                          </li>
                        )}
                      </For>
                    </ul>
                  </div>
                </Show>
              </div>
            )}
          </For>
        </div>
      </Show>
    </Modal>
  );
}
