import { createSignal, Show } from "solid-js";
import Card from "../../components/Card";
import { api } from "../../services/api";
import { useRBAC } from "../../store/rbac";

// ExportGraphPanel is the per-repo "share this graph" section of the
// RepositorySettings page. The admin names the shared graph, adds
// optional tags + a description, and clicks Export; the panel POSTs
// to /{slug}/export-graph, which enqueues an export_graph River job
// (build a whole-repo bundle + gzip + push to the registry). The
// alert surfaces the job id so the admin can poll /tasks/{jobID}.
//
// Gated by graph:export (sysadmin + repoadmin + editor). The panel
// self-hides when the caller lacks the permission so the settings
// page doesn't render a 403-on-click.
//
// Props:
//   repoID  – accessor string  repository UUID
//   slug    – accessor string  repository slug (for the export endpoint)
//   onAlert – (alert) => void  surface export errors / success
export default function ExportGraphPanel(props) {
  const rbac = useRBAC();
  const canExport = () => rbac.hasPermission("graph", "export");

  const [name, setName] = createSignal("");
  const [description, setDescription] = createSignal("");
  const [tags, setTags] = createSignal("");
  const [busy, setBusy] = createSignal(false);
  const [downloading, setDownloading] = createSignal(false);
  const [includeBodies, setIncludeBodies] = createSignal(false);

  const handleExport = async () => {
    const slug = props.slug?.();
    if (!slug) {
      props.onAlert?.({ variant: "error", message: "No repository selected." });
      return;
    }
    setBusy(true);
    try {
      const tagList = tags()
        .split(",")
        .map((t) => t.trim())
        .filter(Boolean);
      const res = await api.exportRepoGraph(slug, {
        name: name().trim(),
        description: description().trim(),
        tags: tagList,
        include_bodies: includeBodies(),
      });
      props.onAlert?.({
        variant: "success",
        message: `Graph export enqueued (job: ${res.job_id}). Poll /tasks/${res.job_id} for completion.`,
      });
      setName("");
      setDescription("");
      setTags("");
    } catch (err) {
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setBusy(false);
    }
  };

  // handleDownload builds the bundle synchronously and saves it as a
  // gzipped JSON file. No registry required — the bundle is built
  // in-process on the server and returned as a Blob. The filename is
  // <slug>.json.gz (the server sets Content-Disposition). The saved
  // file is directly re-importable on any OKT instance via the
  // "Upload bundle" path.
  const handleDownload = async () => {
    const slug = props.slug?.();
    if (!slug) {
      props.onAlert?.({ variant: "error", message: "No repository selected." });
      return;
    }
    setDownloading(true);
    try {
      const blob = await api.downloadRepoGraph(slug, name().trim() || undefined, includeBodies());
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = `${slug}.json.gz`;
      document.body.appendChild(a);
      a.click();
      document.body.removeChild(a);
      URL.revokeObjectURL(url);
      props.onAlert?.({
        variant: "success",
        message:
          "Graph bundle downloaded. Upload it on another instance via the Shared Graphs page.",
      });
    } catch (err) {
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setDownloading(false);
    }
  };

  return (
    <Show when={canExport()}>
      <Card>
        <h3 class="text-lg font-semibold mb-1 dark:text-white">Share Graph</h3>
        <p class="text-sm text-gray-500 dark:text-gray-400 mb-4">
          Export this repository's entire graph — sources, facts, concepts, summaries, syntheses,
          investigations, reports, and embeddings — as a gzipped bundle to the shared knowledge
          registry. Other OKT instances can then import it into a fresh repository in a single task,
          skipping the decomposition and summarization pipeline entirely (zero LLM cost).
        </p>
        <div class="space-y-3">
          <div>
            <label class="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">
              Graph name *
            </label>
            <input
              type="text"
              value={name()}
              onInput={(e) => setName(e.currentTarget.value)}
              disabled={busy()}
              placeholder="e.g. Human Alimentation Graph"
              maxlength={200}
              class="w-full text-sm px-3 py-2 rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-700 dark:text-gray-200 disabled:opacity-50"
            />
          </div>
          <div>
            <label class="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">
              Description
            </label>
            <textarea
              value={description()}
              onInput={(e) => setDescription(e.currentTarget.value)}
              disabled={busy()}
              rows="2"
              placeholder="What this graph covers, so others can decide whether to import it."
              class="w-full text-sm px-3 py-2 rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-700 dark:text-gray-200 disabled:opacity-50"
            />
          </div>
          <div>
            <label class="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">
              Tags (comma-separated)
            </label>
            <input
              type="text"
              value={tags()}
              onInput={(e) => setTags(e.currentTarget.value)}
              disabled={busy()}
              placeholder="scientific, agriculture, climate"
              class="w-full text-sm px-3 py-2 rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-700 dark:text-gray-200 disabled:opacity-50"
            />
            <p class="text-xs text-gray-400 dark:text-gray-500 mt-1">
              Tags help others discover the graph in the Shared Graphs browser.
            </p>
          </div>
          <label class="flex items-start gap-2 text-sm text-gray-700 dark:text-gray-300">
            <input
              type="checkbox"
              checked={includeBodies()}
              onInput={(e) => setIncludeBodies(e.currentTarget.checked)}
              disabled={busy() || downloading()}
              class="mt-0.5 rounded border-gray-300 dark:border-gray-600 dark:bg-gray-900"
            />
            <span>
              Include original PDFs
              <span class="block text-xs text-gray-400 dark:text-gray-500">
                Embeds the stored source body files (PDFs) in the bundle so uploaded documents
                travel with the graph. Increases bundle size significantly for repos with many PDFs.
                Images are always included.
              </span>
            </span>
          </label>
          <div class="flex items-center gap-3">
            <button
              type="button"
              disabled={!name().trim() || busy()}
              onClick={handleExport}
              class="text-sm px-3 py-1.5 rounded border bg-gray-100 text-gray-700 dark:bg-gray-700 dark:text-gray-200 border-gray-300 dark:border-gray-600 hover:bg-gray-200 dark:hover:bg-gray-600 disabled:opacity-50"
            >
              {busy() ? "Exporting…" : "Export Graph to Shared Library"}
            </button>
            <button
              type="button"
              disabled={downloading()}
              onClick={handleDownload}
              class="text-sm px-3 py-1.5 rounded border bg-gray-100 text-gray-700 dark:bg-gray-700 dark:text-gray-200 border-gray-300 dark:border-gray-600 hover:bg-gray-200 dark:hover:bg-gray-600 disabled:opacity-50"
            >
              {downloading() ? "Downloading…" : "Download Graph File"}
            </button>
          </div>
          <p class="text-xs text-gray-400 dark:text-gray-500">
            "Export" pushes the bundle to the shared registry so others can browse + import it.
            "Download" saves the bundle as a gzipped JSON file you can re-import on any OKT instance
            via the Shared Graphs upload path (no registry needed).
          </p>
        </div>
      </Card>
    </Show>
  );
}
