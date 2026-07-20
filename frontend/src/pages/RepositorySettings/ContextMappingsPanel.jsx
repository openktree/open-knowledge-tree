import { createSignal, For, Show } from "solid-js";
import Badge from "../../components/Badge";
import Button from "../../components/Button";
import { api } from "../../services/api";

// ContextMappingsPanel is the per-repo local↔registry context
// mapping card. It shows:
//   1. Unmapped local contexts — each with an inline registry-target
//      <select> so the admin can pick a target and add the mapping
//      in one click (the interactive "choose a target" surface).
//   2. Existing mappings — with Edit (swap the registry target) and
//      Delete actions.
//   3. The pull policy for unmapped registry contexts (skip |
//      auto_add | catch_all + a catch-all context picker).
//
// The registry's canonical vocab (registry_contexts) is sourced from
// the settings payload; when it's empty (registry down/unconfigured),
// the dropdowns fall back to free-text so the admin can still curate
// mappings (the server accepts any non-empty target when the vocab
// is empty).
//
// Props:
//   - repoID:            () => string
//   - mappings:          () => Array<{local_context, registry_context}>
//   - unmappedLocal:     () => string[]
//   - registryContexts:  () => string[]
//   - unmappedPolicy:    () => string  ("skip" | "auto_add" | "catch_all")
//   - catchAllContext:   () => string | null
//   - contexts:          () => Array<{context}>  — local contexts, for the catch-all picker
//   - onChanged:         () => void  — refetch settings after a mutation
//   - onAlert:           (alert) => void
export default function ContextMappingsPanel(props) {
  const [busy, setBusy] = createSignal(false);
  const [editing, setEditing] = createSignal("");
  const [editTarget, setEditTarget] = createSignal("");
  const [newTargets, setNewTargets] = createSignal({});
  const [policyBusy, setPolicyBusy] = createSignal(false);
  const [pendingPolicy, setPendingPolicy] = createSignal("");
  const [pendingCatchAll, setPendingCatchAll] = createSignal("");

  const registryContexts = () => props.registryContexts?.() ?? [];
  const vocabAvailable = () => registryContexts().length > 0;
  const unmapped = () => props.unmappedLocal?.() ?? [];
  const mappings = () => props.mappings?.() ?? [];
  const policy = () => props.unmappedPolicy?.() ?? "skip";
  const catchAll = () => props.catchAllContext?.() ?? "";
  const localContexts = () => props.contexts?.() ?? [];

  const targetFor = (label) => newTargets()[label] ?? "";
  const setTargetFor = (label, value) => setNewTargets({ ...newTargets(), [label]: value });

  const addMapping = async (localCtx, target) => {
    if (!target) return;
    setBusy(true);
    try {
      await api.setRepositoryContextMapping(props.repoID(), {
        local_context: localCtx,
        registry_context: target,
      });
      setNewTargets({ ...newTargets(), [localCtx]: "" });
      props.onChanged?.();
    } catch (err) {
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setBusy(false);
    }
  };

  const saveEdit = async (localCtx) => {
    setBusy(true);
    try {
      await api.setRepositoryContextMapping(props.repoID(), {
        local_context: localCtx,
        registry_context: editTarget(),
      });
      setEditing("");
      props.onChanged?.();
    } catch (err) {
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setBusy(false);
    }
  };

  const del = async (localCtx) => {
    if (!confirm(`Delete mapping for "${localCtx}"?`)) return;
    setBusy(true);
    try {
      await api.deleteRepositoryContextMapping(props.repoID(), localCtx);
      props.onChanged?.();
    } catch (err) {
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setBusy(false);
    }
  };

  const savePolicy = async () => {
    setPolicyBusy(true);
    try {
      const body = { policy: pendingPolicy() };
      if (pendingPolicy() === "catch_all") {
        body.catch_all_context = pendingCatchAll();
      }
      await api.setUnmappedContextPolicy(props.repoID(), body);
      props.onChanged?.();
    } catch (err) {
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setPolicyBusy(false);
    }
  };

  const startPolicyEdit = () => {
    setPendingPolicy(policy());
    setPendingCatchAll(catchAll());
  };

  const targetOptions = () => {
    if (vocabAvailable()) return registryContexts();
    return [];
  };

  return (
    <div class="space-y-4">
      <div>
        <h3 class="text-lg font-semibold dark:text-white">Context Mapping</h3>
        <p class="text-sm text-gray-500 dark:text-gray-400 mt-1">
          Map your local contexts to the registry's canonical vocabulary so contributions and pulls
          stay aligned. Unmapped contexts drift over time; mapping them keeps your custom labels
          interoperable.
        </p>
      </div>

      <Show when={!vocabAvailable()}>
        <div class="text-xs text-yellow-600 dark:text-yellow-400 p-2 rounded bg-yellow-50 dark:bg-yellow-900/20">
          Registry vocabulary unavailable — the registry is not configured or unreachable. Mappings
          are stored but not validated until a registry is connected.
        </div>
      </Show>

      {/* Unmapped contexts — the interactive "pick a target" surface */}
      <Show when={unmapped().length > 0}>
        <div class="p-3 rounded border border-amber-200 dark:border-amber-800 bg-amber-50 dark:bg-amber-900/10">
          <div class="flex items-center gap-2 mb-2">
            <span class="font-medium text-amber-700 dark:text-amber-300">
              Unmapped contexts ({unmapped().length})
            </span>
            <Badge variant="purple">needs attention</Badge>
          </div>
          <p class="text-xs text-amber-600 dark:text-amber-400 mb-3">
            These local contexts have no mapping and aren't in the registry's vocabulary. Pick a
            registry target for each so concepts under them are shared correctly.
          </p>
          <ul class="space-y-2">
            <For each={unmapped()}>
              {(label) => (
                <li class="flex items-center gap-2">
                  <span class="flex-1 text-sm font-medium dark:text-gray-200">{label}</span>
                  <select
                    value={targetFor(label)}
                    onChange={(e) => setTargetFor(label, e.currentTarget.value)}
                    class="text-sm px-2 py-1 rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-700 dark:text-gray-200"
                  >
                    <option value="">Select registry target…</option>
                    <For each={targetOptions()}>{(ctx) => <option value={ctx}>{ctx}</option>}</For>
                    <Show when={!vocabAvailable()}>
                      <option value="__custom">Type a target…</option>
                    </Show>
                  </select>
                  <Show when={!vocabAvailable() && targetFor(label) === "__custom"}>
                    <input
                      type="text"
                      placeholder="registry label"
                      onBlur={(e) => setTargetFor(label, e.currentTarget.value)}
                      class="text-sm px-2 py-1 rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-700 dark:text-gray-200 w-32"
                    />
                  </Show>
                  <Button
                    onClick={() => addMapping(label, targetFor(label))}
                    disabled={busy() || !targetFor(label) || targetFor(label) === "__custom"}
                    loading={busy()}
                  >
                    Map
                  </Button>
                </li>
              )}
            </For>
          </ul>
        </div>
      </Show>

      {/* Existing mappings */}
      <div>
        <div class="text-sm font-medium dark:text-gray-300 mb-2">Mappings</div>
        <Show
          when={mappings().length > 0}
          fallback={
            <p class="text-xs text-gray-400 dark:text-gray-500">
              No mappings yet. Add one from the unmapped list above or by editing a context.
            </p>
          }
        >
          <ul class="divide-y dark:divide-gray-700">
            <For each={mappings()}>
              {(m) => (
                <li class="py-2 flex items-center gap-2">
                  <span class="flex-1 text-sm dark:text-gray-200">
                    <span class="font-medium">{m.local_context}</span>
                    <span class="mx-2 text-gray-400">→</span>
                    <Show when={editing() !== m.local_context}>
                      <span>{m.registry_context}</span>
                    </Show>
                    <Show when={editing() === m.local_context}>
                      <select
                        value={editTarget()}
                        onChange={(e) => setEditTarget(e.currentTarget.value)}
                        class="text-sm px-2 py-1 rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-700 dark:text-gray-200"
                      >
                        <For each={targetOptions()}>
                          {(ctx) => <option value={ctx}>{ctx}</option>}
                        </For>
                      </select>
                    </Show>
                  </span>
                  <Show when={editing() === m.local_context}>
                    <Button
                      onClick={() => saveEdit(m.local_context)}
                      disabled={busy()}
                      loading={busy()}
                    >
                      Save
                    </Button>
                    <Button variant="secondary" onClick={() => setEditing("")} disabled={busy()}>
                      Cancel
                    </Button>
                  </Show>
                  <Show when={editing() !== m.local_context}>
                    <Button
                      variant="secondary"
                      onClick={() => {
                        setEditing(m.local_context);
                        setEditTarget(m.registry_context);
                      }}
                    >
                      Edit
                    </Button>
                    <Button variant="danger" onClick={() => del(m.local_context)} disabled={busy()}>
                      Delete
                    </Button>
                  </Show>
                </li>
              )}
            </For>
          </ul>
        </Show>
      </div>

      {/* Pull policy for unmapped registry contexts */}
      <div class="pt-3 border-t border-gray-100 dark:border-gray-700">
        <div class="text-sm font-medium dark:text-gray-300 mb-2">
          Pull policy for unmapped registry contexts
        </div>
        <p class="text-xs text-gray-500 dark:text-gray-400 mb-3">
          When pulling a concept whose registry context has no inbound mapping:
        </p>
        <Show
          when={pendingPolicy() === "" && !policyBusy()}
          fallback={
            <div class="space-y-2">
              <div class="flex gap-3">
                <label class="text-sm dark:text-gray-300">
                  <input
                    type="radio"
                    name="policy"
                    value="skip"
                    checked={pendingPolicy() === "skip"}
                    onChange={() => setPendingPolicy("skip")}
                    class="mr-1"
                  />
                  Skip
                </label>
                <label class="text-sm dark:text-gray-300">
                  <input
                    type="radio"
                    name="policy"
                    value="auto_add"
                    checked={pendingPolicy() === "auto_add"}
                    onChange={() => setPendingPolicy("auto_add")}
                    class="mr-1"
                  />
                  Auto-add
                </label>
                <label class="text-sm dark:text-gray-300">
                  <input
                    type="radio"
                    name="policy"
                    value="catch_all"
                    checked={pendingPolicy() === "catch_all"}
                    onChange={() => setPendingPolicy("catch_all")}
                    class="mr-1"
                  />
                  Catch-all
                </label>
              </div>
              <Show when={pendingPolicy() === "catch_all"}>
                <select
                  value={pendingCatchAll()}
                  onChange={(e) => setPendingCatchAll(e.currentTarget.value)}
                  class="text-sm px-2 py-1 rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-700 dark:text-gray-200"
                >
                  <option value="">Select local context…</option>
                  <For each={localContexts()}>
                    {(c) => <option value={c.context}>{c.context}</option>}
                  </For>
                </select>
              </Show>
              <div class="flex gap-2">
                <Button
                  onClick={savePolicy}
                  disabled={policyBusy() || (pendingPolicy() === "catch_all" && !pendingCatchAll())}
                  loading={policyBusy()}
                >
                  Save
                </Button>
                <Button
                  variant="secondary"
                  onClick={() => setPendingPolicy("")}
                  disabled={policyBusy()}
                >
                  Cancel
                </Button>
              </div>
            </div>
          }
        >
          <div class="flex items-center gap-3 text-sm dark:text-gray-300">
            <span class="font-medium capitalize">{policy()}</span>
            <Show when={policy() === "catch_all" && catchAll()}>
              <span class="text-gray-400">→ {catchAll()}</span>
            </Show>
            <Button variant="secondary" onClick={startPolicyEdit}>
              Change
            </Button>
          </div>
        </Show>
      </div>
    </div>
  );
}
