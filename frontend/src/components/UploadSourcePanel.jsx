import { createSignal, Show } from "solid-js";
import { api } from "../services/api";
import Alert from "./Alert";
import Button from "./Button";
import Card from "./Card";

// UploadSourcePanel is a shared UI for uploading a file (PDF/HTML/MD/TXT)
// or pasting raw text to create a source. It calls the per-repo
// /sources/upload endpoint, which parses the content in-process and
// enqueues decomposition. When `invID` is supplied, the server
// atomically links the new source to that investigation.
//
// Props:
//   slug      — repository slug (required)
//   invID     — optional investigation UUID; when set, passed as
//               investigation_id so the source is linked on creation
//   onDone    — callback fired after a successful upload (the parent
//               usually refetches its list)
export default function UploadSourcePanel(props) {
  const [mode, setMode] = createSignal("file");
  const [file, setFile] = createSignal(null);
  const [text, setText] = createSignal("");
  const [title, setTitle] = createSignal("");
  const [kind, setKind] = createSignal("uploaded");
  const [busy, setBusy] = createSignal(false);
  const [alert, setAlert] = createSignal(null);

  const reset = () => {
    setFile(null);
    setText("");
    setTitle("");
    setKind("uploaded");
  };

  const handleSubmit = async (e) => {
    e.preventDefault();
    if (!props.slug) return;
    setBusy(true);
    setAlert(null);
    try {
      let res;
      if (mode() === "file") {
        const f = file();
        if (!f) {
          setAlert({ variant: "error", message: "Choose a file first." });
          setBusy(false);
          return;
        }
        res = await api.uploadSourceFile(props.slug, f, kind(), props.invID || "");
      } else {
        if (!text().trim()) {
          setAlert({ variant: "error", message: "Text cannot be empty." });
          setBusy(false);
          return;
        }
        res = await api.uploadSourceText(props.slug, text(), title(), kind(), props.invID || "");
      }
      const linked = res.investigation_linked ? " and linked to investigation" : "";
      setAlert({ variant: "success", message: `Source uploaded${linked}. Decomposition queued.` });
      reset();
      props.onDone?.();
    } catch (err) {
      setAlert({ variant: "error", message: err.message });
    } finally {
      setBusy(false);
    }
  };

  const body = () => (
    <>
      <div class="flex gap-2 mb-4">
        <button
          type="button"
          onClick={() => setMode("file")}
          class={`px-3 py-1 text-sm rounded border ${
            mode() === "file"
              ? "bg-primary text-white border-primary"
              : "text-text-muted border-border hover:text-text-base"
          }`}
        >
          File
        </button>
        <button
          type="button"
          onClick={() => setMode("text")}
          class={`px-3 py-1 text-sm rounded border ${
            mode() === "text"
              ? "bg-primary text-white border-primary"
              : "text-text-muted border-border hover:text-text-base"
          }`}
        >
          Raw text
        </button>
      </div>

      <Alert
        variant={alert()?.variant}
        message={alert()?.message}
        onDismiss={() => setAlert(null)}
      />

      <form onSubmit={handleSubmit} class="space-y-3">
        <Show when={mode() === "file"}>
          <input
            type="file"
            accept=".pdf,.html,.htm,.md,.markdown,.txt"
            onChange={(e) => setFile(e.currentTarget.files?.[0] || null)}
            class="block w-full text-sm text-text-base"
          />
        </Show>
        <Show when={mode() === "text"}>
          <input
            type="text"
            value={title()}
            onInput={(e) => setTitle(e.currentTarget.value)}
            placeholder="Title (optional)"
            class="w-full px-3 py-2 border border-border rounded text-sm bg-surface text-text-base placeholder-text-muted focus:outline-none focus:ring-2 focus:ring-primary"
          />
          <textarea
            value={text()}
            onInput={(e) => setText(e.currentTarget.value)}
            placeholder="Paste raw text or markdown here..."
            rows="8"
            class="w-full px-3 py-2 border border-border rounded text-sm font-mono bg-surface text-text-base placeholder-text-muted focus:outline-none focus:ring-2 focus:ring-primary"
          />
        </Show>

        <div class="flex gap-2 items-center">
          <select
            value={kind()}
            onChange={(e) => setKind(e.currentTarget.value)}
            class="px-3 py-2 border border-border rounded text-sm bg-surface text-text-base"
          >
            <option value="uploaded">uploaded</option>
            <option value="paper">paper</option>
            <option value="homepage">homepage</option>
            <option value="dataset">dataset</option>
            <option value="code">code</option>
            <option value="other">other</option>
          </select>
          <Button type="submit" loading={busy()} loadingText="Uploading...">
            Upload
          </Button>
          <Button type="button" variant="secondary" onClick={() => props.onCancel?.()}>
            Cancel
          </Button>
        </div>
      </form>
    </>
  );

  return (
    <Show
      when={props.bare}
      fallback={
        <Card class="mb-6">
          <h2 class="text-lg font-semibold mb-1 text-text-base">Upload a source</h2>
          <p class="text-sm text-text-muted mb-4">
            Upload a file (PDF, HTML, Markdown, TXT) or paste raw text. The content is parsed and
            decomposed into facts automatically.
          </p>
          {body()}
        </Card>
      }
    >
      {body()}
    </Show>
  );
}
