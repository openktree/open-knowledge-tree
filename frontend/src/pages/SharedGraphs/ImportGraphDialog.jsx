import { createSignal, Show } from "solid-js";
import { IMPORT_MODE_EXISTING, IMPORT_MODE_NEW, MODE_LABELS } from "./constants";

// ImportGraphDialog is the modal the Shared Graphs page opens when
// the user clicks "Import" on a shared graph. It offers two modes:
//   - new: create a fresh repository from the graph (name + slug
//     required; the server creates the repo + seeds default settings
//     before enqueuing the import task).
//   - existing: import into the current repository (merge semantics;
//     sources/facts/concepts dedup by url/content_hash/(name,context),
//     summaries/syntheses imported verbatim and skipped on conflict).
export default function ImportGraphDialog(props) {
  const [mode, setMode] = createSignal(IMPORT_MODE_NEW);
  const [name, setName] = createSignal("");
  const [slug, setSlug] = createSignal("");
  const [description, setDescription] = createSignal("");
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal("");

  const canSubmit = () => {
    if (busy()) return false;
    if (mode() === IMPORT_MODE_NEW) return name().trim() && slug().trim();
    return !!props.currentRepoSlug();
  };

  const handleSubmit = async () => {
    setBusy(true);
    setError("");
    try {
      if (mode() === IMPORT_MODE_NEW) {
        const res = await api.importGraphToNewRepo({
          registry_graph_id: props.graph().id,
          name: name().trim(),
          slug: slug()
            .trim()
            .toLowerCase()
            .replace(/[^a-z0-9-]/g, "-"),
          description: description().trim(),
        });
        props.onSuccess({
          message: `Import enqueued — new repository "${res.slug}" (job: ${res.job_id})`,
          repository_id: res.repository_id,
          slug: res.slug,
        });
      } else {
        const res = await api.importGraphToExisting(props.currentRepoSlug(), {
          registry_graph_id: props.graph().id,
        });
        props.onSuccess({
          message: `Import enqueued into "${props.currentRepoSlug()}" (job: ${res.job_id})`,
        });
      }
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="fixed inset-0 bg-black/50 flex items-center justify-center z-30 p-4">
      <div class="bg-surface border border-border rounded-lg shadow-xl max-w-md w-full p-5">
        <h3 class="text-base font-semibold text-text-base mb-1">Import shared graph</h3>
        <p class="text-sm text-text-muted mb-4 truncate">{props.graph()?.name}</p>

        <fieldset class="space-y-2 mb-4">
          <label class="flex items-start gap-2 text-sm">
            <input
              type="radio"
              name="import-mode"
              checked={mode() === IMPORT_MODE_NEW}
              onChange={() => setMode(IMPORT_MODE_NEW)}
              class="mt-0.5"
            />
            <span>{MODE_LABELS[IMPORT_MODE_NEW]}</span>
          </label>
          <label class="flex items-start gap-2 text-sm">
            <input
              type="radio"
              name="import-mode"
              checked={mode() === IMPORT_MODE_EXISTING}
              onChange={() => setMode(IMPORT_MODE_EXISTING)}
              class="mt-0.5"
            />
            <span>{MODE_LABELS[IMPORT_MODE_EXISTING]}</span>
          </label>
        </fieldset>

        <Show when={mode() === IMPORT_MODE_NEW}>
          <div class="space-y-3 mb-4">
            <div>
              <label class="block text-xs font-medium text-text-muted mb-1">Name *</label>
              <input
                type="text"
                value={name()}
                onInput={(e) => setName(e.currentTarget.value)}
                class="w-full text-sm border border-border rounded-md px-2 py-1.5 bg-surface text-text-base"
                placeholder="My imported graph"
              />
            </div>
            <div>
              <label class="block text-xs font-medium text-text-muted mb-1">Slug *</label>
              <input
                type="text"
                value={slug()}
                onInput={(e) => setSlug(e.currentTarget.value)}
                class="w-full text-sm border border-border rounded-md px-2 py-1.5 bg-surface text-text-base"
                placeholder="my-imported-graph"
              />
            </div>
            <div>
              <label class="block text-xs font-medium text-text-muted mb-1">Description</label>
              <textarea
                value={description()}
                onInput={(e) => setDescription(e.currentTarget.value)}
                class="w-full text-sm border border-border rounded-md px-2 py-1.5 bg-surface text-text-base"
                rows="2"
              />
            </div>
          </div>
        </Show>

        <Show when={mode() === IMPORT_MODE_EXISTING}>
          <p class="text-xs text-text-muted mb-4 p-2 bg-primary-soft rounded">
            Imported sources/facts/concepts will be deduplicated against the existing repository.
            Summaries and syntheses are imported verbatim and skipped on conflict — you can trigger
            regeneration from the UI afterwards.
          </p>
        </Show>

        <Show when={error()}>
          <p class="text-sm text-danger mb-3">{error()}</p>
        </Show>

        <div class="flex justify-end gap-2">
          <button
            type="button"
            class="text-sm px-3 py-1.5 rounded border border-border bg-surface text-text-base hover:bg-primary-soft transition"
            onClick={props.onClose}
            disabled={busy()}
          >
            Cancel
          </button>
          <button
            type="button"
            class="text-sm px-3 py-1.5 rounded bg-primary-fg text-white hover:opacity-90 transition disabled:opacity-50"
            onClick={handleSubmit}
            disabled={!canSubmit()}
          >
            {busy() ? "Importing…" : "Import"}
          </button>
        </div>
      </div>
    </div>
  );
}
