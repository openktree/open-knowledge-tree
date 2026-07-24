import { A } from "@solidjs/router";
import { createResource, For, Show, Suspense } from "solid-js";
import { api } from "../services/api";
import Badge from "./Badge";
import FactBadges from "./FactBadges";
import ImageFromUrl from "./ImageFromUrl";
import Loading from "./Loading";
import Modal from "./Modal";

// CitationModal renders an inline fact or concept citation in a
// modal dialog (instead of navigating away to the detail page). It
// is opened by CitedView / DefinitionPanel / SummaryPanel when the
// user clicks a normalized [/facts/{id}] or [/concepts/{id}] link.
//
// The modal fetches the fact or concept detail on open and renders:
//   - FACT: the fact text, kind/status/source-count badges, the image
//     (for image facts), the source URLs, and a "View full fact page"
//     link. On a 404 (the fact was deleted or the citation UUID is
//     hallucinated), the modal shows an error message with the UUID
//     instead of freezing on "Loading...".
//   - CONCEPT: the canonical name + context, the definition
//     (synthesis) if one exists, and a "View full concept page" link.
//     On a 404, same error treatment.
//
// Props:
//   - open:    boolean
//   - onClose: () => void
//   - kind:    "fact" | "concept"
//   - id:      UUID string
//   - slug:    repo slug
export default function CitationModal(props) {
  const [data] = createResource(
    () => (props.open && props.id ? { kind: props.kind, id: props.id, slug: props.slug } : null),
    async ({ kind, id, slug }) => {
      if (kind === "concept") {
        try {
          const concept = await api.getConcept(slug, id);
          let definition = null;
          try {
            definition = await api.getConceptDefinition(slug, id);
          } catch {
            // 404 when no synthesis exists yet — fine.
          }
          return { kind: "concept", concept, definition };
        } catch (err) {
          return { kind: "concept", error: err?.message || "fetch failed", id };
        }
      }
      // api.getFact returns {fact, sources, source_count, concepts,
      // concept_count}; the fact row is nested under "fact", and
      // "sources" carries the source URL list.
      try {
        const res = await api.getFact(slug, id);
        return {
          kind: "fact",
          fact: res.fact,
          sources: res.sources || [],
          sourceCount: res.source_count,
        };
      } catch (err) {
        return { kind: "fact", error: err?.message || "fetch failed", id };
      }
    },
  );

  const fact = () => (data()?.kind === "fact" && !data()?.error ? data().fact : null);
  const sources = () => (data()?.kind === "fact" ? data().sources || [] : []);
  const concept = () => (data()?.kind === "concept" && !data()?.error ? data().concept : null);
  const definition = () => (data()?.kind === "concept" ? data().definition : null);
  const isImageFact = () => fact()?.fact_kind === "image" && !!fact()?.image_url;
  const errorMsg = () => data()?.error;

  const title = () => {
    if (props.kind === "concept") return "Concept";
    return "Fact";
  };

  return (
    <Modal open={props.open} onClose={props.onClose} title={title()}>
      <Suspense fallback={<Loading message="Loading..." />}>
        <Show when={!data.loading} fallback={<Loading message="Loading..." />}>
          {/* ── ERROR (404 / fetch failure) ── */}
          <Show when={errorMsg()}>
            <div class="space-y-3">
              <p class="text-red-600 dark:text-red-400 text-sm font-medium">
                Could not load this {props.kind}.
              </p>
              <p class="text-xs text-text-muted">
                {errorMsg()} — the {props.kind} may have been deleted, or the citation UUID is
                invalid.
              </p>
              <div class="text-xs text-text-muted font-mono break-all">{props.id}</div>
              <div class="pt-2 border-t border-border">
                <A
                  href={`/${props.slug}/${props.kind === "concept" ? "concepts" : "facts"}/${props.id}`}
                  class="text-xs text-primary-fg hover:underline"
                  onClick={() => props.onClose?.()}
                >
                  Try opening the full {props.kind} page →
                </A>
              </div>
            </div>
          </Show>

          {/* ── FACT ── */}
          <Show when={fact()}>
            {(f) => (
              <div class="space-y-3">
                <Show when={isImageFact()}>
                  <ImageFromUrl
                    imageUrl={f().image_url}
                    alt=""
                    class="max-h-60 rounded border border-border"
                  />
                </Show>
                <div class="text-text-base text-sm leading-relaxed">{f().text}</div>
                <FactBadges
                  fact={{
                    id: f().id,
                    status: f().status,
                    fact_kind: f().fact_kind,
                    image_url: f().image_url,
                    source_count: f().source_count,
                  }}
                  slug={props.slug}
                />
                {/* Source URLs */}
                <Show when={sources().length > 0}>
                  <div class="border-t border-border pt-2">
                    <div class="text-xs text-text-muted mb-1">Sources:</div>
                    <ul class="space-y-1">
                      <For each={sources()}>
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
                <div class="pt-2 border-t border-border">
                  <A
                    href={`/${props.slug}/facts/${f().id}`}
                    class="text-xs text-primary-fg hover:underline"
                    onClick={() => props.onClose?.()}
                  >
                    View full fact page →
                  </A>
                </div>
              </div>
            )}
          </Show>

          {/* ── CONCEPT ── */}
          <Show when={concept()}>
            {(c) => (
              <div class="space-y-3">
                <div class="flex items-center gap-2 flex-wrap">
                  <span class="text-base font-semibold text-text-base">{c().canonical_name}</span>
                  <Show when={(c().contexts || []).length > 0}>
                    <Badge variant="blue">{c().contexts[0].context}</Badge>
                  </Show>
                  <Show when={c().description}>
                    <span class="text-xs text-text-muted">{c().description}</span>
                  </Show>
                </div>
                <Show
                  when={definition()?.synthesis?.content}
                  fallback={
                    <p class="text-text-muted italic text-xs">No definition synthesized yet.</p>
                  }
                >
                  <div class="border border-border rounded p-3 max-h-50 overflow-y-auto">
                    <div class="text-xs text-text-muted mb-1">Definition</div>
                    <div class="text-sm text-text-base whitespace-pre-wrap">
                      {definition().synthesis.content.slice(0, 600)}
                      <Show when={definition().synthesis.content.length > 600}>
                        <span class="text-text-muted">…</span>
                      </Show>
                    </div>
                  </div>
                </Show>
                <div class="pt-2 border-t border-border">
                  <A
                    href={`/${props.slug}/concepts/${props.id}`}
                    class="text-xs text-primary-fg hover:underline"
                    onClick={() => props.onClose?.()}
                  >
                    View full concept page →
                  </A>
                </div>
              </div>
            )}
          </Show>
        </Show>
      </Suspense>
    </Modal>
  );
}
