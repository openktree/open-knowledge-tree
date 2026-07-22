import { TAG_PRESETS } from "./constants";

export default function SharedGraphsContent(props) {
  const rows = () => props.graphs()?.graphs || [];
  return (
    <>
      <div class="flex items-center justify-between mb-3 gap-3 flex-wrap">
        <h2 class="text-lg font-semibold dark:text-white">Shared knowledge graphs</h2>
        <div class="flex items-center gap-2 flex-wrap">
          <button
            type="button"
            class="text-xs px-2.5 py-1.5 rounded border border-border bg-surface text-text-base hover:bg-primary-soft transition"
            onClick={props.onUpload}
          >
            Upload Bundle
          </button>
          <select
            class="text-sm border border-border rounded-md px-2 py-1.5 bg-surface text-text-base"
            value={props.tag()}
            onChange={(e) => props.onTagChange(e.currentTarget.value)}
          >
            <option value="">All tags</option>
            {TAG_PRESETS.map((t) => (
              <option value={t}>{t}</option>
            ))}
          </select>
          <input
            type="text"
            placeholder="Search name or description..."
            value={props.search()}
            onInput={(e) => props.onSearch(e.currentTarget.value)}
            class="text-sm border border-border rounded-md px-2 py-1.5 bg-surface text-text-base w-64"
          />
        </div>
      </div>
      <p class="text-sm text-gray-500 dark:text-gray-400 mb-4">
        Graphs shared by other OKT instances. Each bundle contains a full repository's sources,
        facts, concepts, summaries, syntheses, investigations, reports, and embeddings — import one
        into a new repository to skip the decomposition and summarization pipeline entirely (zero
        LLM cost).
      </p>

      {props.loading() ? (
        <p class="text-sm text-gray-400 dark:text-gray-500">Loading shared graphs...</p>
      ) : rows().length === 0 ? (
        <p class="text-sm text-gray-400 dark:text-gray-500">
          {props.search() || props.tag()
            ? "No shared graphs match your filters."
            : "No shared graphs available. Export a repository's graph to share it."}
        </p>
      ) : (
        <>
          <p class="text-xs text-gray-500 dark:text-gray-400 mb-3">
            Showing {props.offset() + 1}–{Math.min(props.offset() + props.limit, props.total())} of{" "}
            {props.total().toLocaleString()}
          </p>
          <div class="space-y-2">
            {rows().map((g) => (
              <div
                class="border border-border rounded-md p-3 bg-surface hover:bg-primary-soft transition cursor-pointer"
                onClick={() => props.onOpenDetail(g)}
              >
                <div class="flex items-center justify-between gap-3">
                  <div class="min-w-0 flex-1">
                    <h3 class="text-sm font-medium text-text-base truncate">{g.name}</h3>
                    {g.description ? (
                      <p class="text-xs text-text-muted truncate mt-0.5">{g.description}</p>
                    ) : null}
                    <div class="flex items-center gap-3 mt-1 text-xs text-text-muted">
                      <span>{g.source_count} sources</span>
                      <span>{g.fact_count} facts</span>
                      <span>{g.concept_count} concepts</span>
                      {g.owner ? <span>by {g.owner}</span> : null}
                    </div>
                    {g.tags && g.tags.length > 0 ? (
                      <div class="flex flex-wrap gap-1 mt-1.5">
                        {g.tags.map((t) => (
                          <span class="text-[10px] px-1.5 py-0.5 rounded bg-primary-soft text-text-muted">
                            {t}
                          </span>
                        ))}
                      </div>
                    ) : null}
                  </div>
                  <button
                    type="button"
                    class="text-xs px-2.5 py-1.5 rounded border border-border bg-surface text-text-base hover:bg-primary-soft transition flex-shrink-0"
                    onClick={(e) => {
                      e.stopPropagation();
                      props.onImport(g);
                    }}
                  >
                    Import
                  </button>
                </div>
              </div>
            ))}
          </div>
        </>
      )}
    </>
  );
}
