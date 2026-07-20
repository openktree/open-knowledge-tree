import { createSignal, For, Show } from "solid-js";
import Card from "../../components/Card";
import { api } from "../../services/api";

const CONTENT_TYPES = [
  { value: "document", label: "Documents (uploaded files)" },
  { value: "url", label: "URLs (web pages)" },
  { value: "doi", label: "DOIs (academic works)" },
];

export default function ContentTypesPanel(props) {
  const [busy, setBusy] = createSignal(false);
  const [result, setResult] = createSignal(null);
  // Local copy of the selected set. null = allow all (the default);
  // a Set object = restrict to its members.
  const [selected, setSelected] = createSignal(null);

  // Sync local state from the server-provided value. createSignal
  // initializes null; this effect runs when the resource loads.
  const serverVal = () => props.allowedContentTypes?.();
  if (selected() === null && serverVal() !== undefined && serverVal() !== null) {
    setSelected(new Set(serverVal()));
  }

  const allowAll = () => selected() === null;
  const isChecked = (kind) => (allowAll() ? true : selected().has(kind));

  const toggleAllowAll = () => {
    if (allowAll()) {
      // Switching from allow-all to restrict: start with all three
      // checked so the admin can uncheck the ones they want to deny.
      setSelected(new Set(["document", "url", "doi"]));
    } else {
      setSelected(null);
    }
  };

  const toggleKind = (kind) => {
    const next = new Set(selected());
    if (next.has(kind)) {
      next.delete(kind);
    } else {
      next.add(kind);
    }
    // An empty set would reject everything; guard against it by
    // falling back to allow-all when the last box is unchecked in
    // restrict mode. The server rejects an empty array with a 400,
    // so this keeps the UI honest.
    if (next.size === 0) {
      setSelected(null);
    } else {
      setSelected(next);
    }
  };

  const handleSave = async () => {
    setBusy(true);
    setResult(null);
    try {
      const body = allowAll()
        ? { allowed_content_types: null }
        : { allowed_content_types: Array.from(selected()) };
      await api.setRepositoryContentTypes(props.repoID(), body);
      setResult({ variant: "success", message: "Content types saved." });
      props.onChanged?.();
    } catch (err) {
      setResult({ variant: "error", message: err.message });
    } finally {
      setBusy(false);
    }
  };

  const dirty = () => {
    const sv = serverVal();
    if (allowAll()) {
      return sv !== undefined && sv !== null;
    }
    if (sv === undefined || sv === null) {
      return true;
    }
    const a = Array.from(selected()).sort().join(",");
    const b = [...sv].sort().join(",");
    return a !== b;
  };

  return (
    <Card>
      <h3 class="text-lg font-semibold mb-3 dark:text-white">Allowed Content Types</h3>
      <p class="text-sm text-gray-500 dark:text-gray-400 mb-4">
        Restrict what kinds of sources this repository accepts. A strictly scientific repo can allow
        only DOIs; a documents-only repo can allow only uploads. "Allow all" (the default) accepts
        documents, URLs, and DOIs.
      </p>

      <div class="mb-4 p-3 border rounded dark:border-gray-700 bg-gray-50 dark:bg-gray-800/50">
        <label class="flex items-center gap-2 text-sm font-medium dark:text-gray-200 mb-3">
          <input
            type="checkbox"
            checked={allowAll()}
            onInput={toggleAllowAll}
            class="rounded border-gray-300 dark:border-gray-600 dark:bg-gray-900"
          />
          Allow all content types (no restriction)
        </label>

        <Show when={!allowAll()}>
          <div class="space-y-2 ml-5">
            <For each={CONTENT_TYPES}>
              {(ct) => (
                <label class="flex items-center gap-2 text-sm dark:text-gray-300">
                  <input
                    type="checkbox"
                    checked={isChecked(ct.value)}
                    onInput={() => toggleKind(ct.value)}
                    class="rounded border-gray-300 dark:border-gray-600 dark:bg-gray-900"
                  />
                  {ct.label}
                </label>
              )}
            </For>
          </div>
        </Show>

        <div class="mt-4 flex items-center gap-3">
          <button
            type="button"
            disabled={!dirty() || busy()}
            onClick={handleSave}
            class="text-sm px-3 py-1.5 rounded border bg-gray-100 text-gray-700 dark:bg-gray-700 dark:text-gray-200 border-gray-300 dark:border-gray-600 hover:bg-gray-200 dark:hover:bg-gray-600 disabled:opacity-50"
          >
            {busy() ? "Saving…" : "Save Content Types"}
          </button>
          <Show when={result()}>
            {(r) => (
              <span
                class={`text-sm px-3 py-1.5 rounded ${
                  r().variant === "error"
                    ? "bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300"
                    : "bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-300"
                }`}
              >
                {r().message}
              </span>
            )}
          </Show>
        </div>
      </div>
    </Card>
  );
}
