import { createSignal, For, Show } from "solid-js";
import Badge from "../../components/Badge";
import Card from "../../components/Card";
import { api } from "../../services/api";
import { PROVIDER_KIND_LABEL } from "./constants";

// ProvidersPanel renders the live provider catalog for a repo,
// each tagged with whether it's enabled. Toggling calls
// PUT /settings/providers. Orphans (stored but not live) are
// rendered greyed so the admin sees the drift.
//
// Props:
//   - repoID:   () => string
//   - providers: () => Array<{kind,id,name,stored,enabled,orphaned}>
//   - onChanged: () => void  — refetch settings after a toggle
//   - onAlert:   (alert) => void
export default function ProvidersPanel(props) {
  const [busy, setBusy] = createSignal("");

  const toggle = async (p) => {
    setBusy(p.id);
    try {
      await api.setRepositoryProvider(props.repoID(), {
        provider_kind: p.kind,
        provider_id: p.id,
        enabled: !p.enabled,
      });
      props.onChanged?.();
    } catch (err) {
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setBusy("");
    }
  };

  const groups = () => {
    const all = props.providers() || [];
    const map = { search: [], resolution: [] };
    for (const p of all) {
      (map[p.kind] || (map[p.kind] = [])).push(p);
    }
    return map;
  };

  return (
    <Card>
      <h3 class="text-lg font-semibold mb-3 dark:text-white">Providers</h3>
      <p class="text-sm text-gray-500 dark:text-gray-400 mb-4">
        Toggle which search and resolution providers this repository allows. Disabled providers
        reject searches and fetches for this repo.
      </p>
      <For each={["search", "resolution"]}>
        {(kind) => (
          <div class="mb-4">
            <h4 class="text-sm font-medium mb-2 dark:text-gray-300">
              {PROVIDER_KIND_LABEL[kind] || kind}
            </h4>
            <Show
              when={(groups()[kind] || []).length > 0}
              fallback={<p class="text-xs text-gray-400">None live in this deployment.</p>}
            >
              <ul class="space-y-1">
                <For each={groups()[kind]}>
                  {(p) => (
                    <li class="flex items-center justify-between py-1">
                      <span class="flex items-center gap-2 text-sm dark:text-gray-200">
                        {p.name}
                        <Show when={p.orphaned}>
                          <Badge variant="gray">unavailable</Badge>
                        </Show>
                      </span>
                      <button
                        type="button"
                        disabled={busy() === p.id || p.orphaned}
                        onClick={() => toggle(p)}
                        class={`text-xs px-2 py-1 rounded border ${
                          p.enabled
                            ? "bg-green-100 text-green-700 dark:bg-green-900/40 dark:text-green-300 border-green-300 dark:border-green-700"
                            : "bg-gray-100 text-gray-500 dark:bg-gray-700 dark:text-gray-400 border-gray-300 dark:border-gray-600"
                        } disabled:opacity-50`}
                      >
                        {p.enabled ? "Enabled" : "Disabled"}
                      </button>
                    </li>
                  )}
                </For>
              </ul>
            </Show>
          </div>
        )}
      </For>
    </Card>
  );
}
