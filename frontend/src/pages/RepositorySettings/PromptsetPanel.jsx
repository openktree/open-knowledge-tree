import { createMemo, createResource, createSignal, For, Show } from "solid-js";
import { api } from "../../services/api";
import Card from "../../components/Card";
import { BUILTIN_SOURCE } from "../Promptsets/constants";

// PromptsetPanel is the per-repo promptset selection section of the
// RepositorySettings page. It loads the repo's active + accepted
// hashes plus the catalog of available promptsets, and lets the
// admin pick the active philosophy + the additionally-accepted set
// (the cache-admit set for registry pull).
//
// The "Additionally accepted" list groups promptsets by their
// registry_hash (the compatibility hash over the 4 shared phases)
// because that's the hash the pull filter actually compares
// against — two promptsets with the same registry_hash are
// interchangeable on pull, so accepting one accepts the whole
// compatibility class. The UI surfaces this so the admin sees
// "accept all compatible with <name>" rather than a flat per-hash
// list.
//
// Props:
//   repoID  – accessor string  repository UUID (or slug)
//   onAlert – (alert) => void
export default function PromptsetPanel(props) {
  const [ps, { refetch }] = createResource(props.repoID, (id) =>
    id ? api.getRepositoryPromptset(id).catch((e) => {
      props.onAlert?.({ variant: "error", message: e.message });
      return null;
    }) : null
  );
  const [catalog, { refetch: refetchCatalog }] = createResource(() => api.listPromptsets().catch(() => []));
  const [busy, setBusy] = createSignal(false);

  const active = () => ps()?.active_hash ?? "";
  const accepted = () => ps()?.accepted_hashes ?? [];
  const effective = () => ps()?.effective_hash ?? "";
  const globalDefault = () => ps()?.global_default_hash ?? "";
  const activePromptset = createMemo(() =>
    (catalog() ?? []).find((p) => p.hash === active()) ?? null
  );
  // Catalog grouped by registry_hash. Each group has a representative
  // name (the built-in's name when present, else the first member's
  // name) and the list of members. Sorted so the built-in's group
  // (compatible with default) is first.
  const groups = createMemo(() => {
    const list = catalog() ?? [];
    const byHash = new Map();
    for (const p of list) {
      const k = p.registry_hash || p.hash;
      if (!byHash.has(k)) byHash.set(k, []);
      byHash.get(k).push(p);
    }
    const out = [];
    for (const [k, members] of byHash) {
      const bi = members.find((p) => p.source === BUILTIN_SOURCE);
      const rep = bi ?? members[0];
      out.push({ registryHash: k, name: rep.name, members });
    }
    // Built-in's group first, then alphabetical.
    out.sort((a, b) => {
      const aBi = a.members.some((p) => p.source === BUILTIN_SOURCE) ? 0 : 1;
      const bBi = b.members.some((p) => p.source === BUILTIN_SOURCE) ? 0 : 1;
      if (aBi !== bBi) return aBi - bBi;
      return a.name.localeCompare(b.name);
    });
    return out;
  });
  // A group is accepted when any of its members' full hashes is in
  // the repo's accepted set. The active hash is always accepted.
  const isGroupAccepted = (g) => g.members.some((p) => accepted().includes(p.hash) || p.hash === active());

  const isAccepted = (hash) => accepted().includes(hash);

  const save = async (nextActive, nextAccepted) => {
    setBusy(true);
    try {
      await api.setRepositoryPromptset(props.repoID(), {
        active_hash: nextActive || null,
        accepted_hashes: nextAccepted,
      });
      props.onAlert?.({ variant: "success", message: "Promptset updated." });
      refetch();
    } catch (err) {
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setBusy(false);
    }
  };

  const changeActive = (hash) => {
    // Keep the active hash in the accepted set.
    const next = new Set(accepted());
    if (hash) next.add(hash);
    save(hash, Array.from(next));
  };

  const toggleAccepted = (hash, checked) => {
    const next = new Set(accepted());
    if (checked) next.add(hash);
    else next.delete(hash);
    // The active hash is always accepted; don't let the caller
    // remove it via the checkbox.
    if (active() && !next.has(active())) next.add(active());
    save(active(), Array.from(next));
  };

  // Toggle every member of a compatibility group. When turning the
  // group on, add every member's full hash to the accepted set (so
  // the repo pulls decompositions tagged with any of them — though
  // the pull filter compares on the registry_hash, listing all
  // members keeps the catalog honest). When turning off, remove
  // every member except the active hash (always retained).
  const toggleGroup = (group, checked) => {
    const next = new Set(accepted());
    for (const p of group.members) {
      if (checked) next.add(p.hash);
      else if (p.hash !== active()) next.delete(p.hash);
    }
    save(active(), Array.from(next));
  };

  return (
    <Card>
      <h3 class="text-lg font-semibold mb-1 dark:text-white">Promptset</h3>
      <p class="text-sm text-gray-500 dark:text-gray-400 mb-4">
        Choose the philosophy this repository decomposes under. The active promptset runs for new
        decompositions; the accepted set controls which foreign decompositions the registry cache may
        pull in. Two promptsets that differ only in synthesis / summarization / posture / image-picker
        share a <strong>compatibility hash</strong> and are interchangeable on pull — accept the whole
        group, not just one.
      </p>
      <Show when={!ps.loading && !catalog.loading}>
        <div class="space-y-4">
          <div>
            <label class="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">
              Active promptset
            </label>
            <select
              disabled={busy()}
              value={active()}
              onChange={(e) => changeActive(e.currentTarget.value)}
              class="text-sm px-3 py-2 rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-700 dark:text-gray-200 disabled:opacity-50"
            >
              <option value="">
                Inherit global default{globalDefault() ? ` (${globalDefault().slice(0, 12)}…)` : ""}
              </option>
              <For each={catalog() ?? []}>
                {(p) => <option value={p.hash}>{p.name} ({p.source})</option>}
              </For>
            </select>
            <p class="text-xs text-gray-400 dark:text-gray-500 mt-1">
              Effective: <span class="font-mono">{effective().slice(0, 16)}…</span>
              <Show when={activePromptset()?.registry_hash}>
                {" — compatibility: "}
                <span class="font-mono">{activePromptset().registry_hash.slice(0, 16)}…</span>
              </Show>
            </p>
          </div>
          <div>
            <label class="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">
              Additionally accepted (registry pull)
            </label>
            <p class="text-xs text-gray-400 dark:text-gray-500 mb-2">
              Decompositions tagged with any of these hashes (or their compatibility class)
              may be pulled from the registry. The active hash is always accepted. The
              built-in compatibility class is always accepted.
            </p>
            <div class="space-y-2">
              <For each={groups()}>
                {(g) => (
                  <div class="rounded border border-gray-200 dark:border-gray-700 px-3 py-2">
                    <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
                      <input
                        type="checkbox"
                        disabled={busy() || g.members.some((p) => p.hash === active())}
                        checked={isGroupAccepted(g)}
                        onChange={(e) => toggleGroup(g, e.currentTarget.checked)}
                        class="rounded"
                      />
                      <span class="font-medium">{g.name}</span>
                      <span class="text-xs text-gray-400 dark:text-gray-500">
                        ({g.members.length} compatible)
                      </span>
                      <span class="font-mono text-xs text-gray-400 dark:text-gray-500">
                        {g.registryHash ? g.registryHash.slice(0, 12) + "…" : ""}
                      </span>
                      <Show when={g.members.some((p) => p.source === BUILTIN_SOURCE)}>
                        <span class="px-1.5 py-0.5 text-xs rounded bg-green-100 dark:bg-green-900 text-green-700 dark:text-green-200">
                          default class
                        </span>
                      </Show>
                    </label>
                    <Show when={g.members.length > 1}>
                      <ul class="mt-1 ml-6 list-disc text-xs text-gray-500 dark:text-gray-400 space-y-0.5">
                        <For each={g.members}>
                          {(p) => (
                            <li>
                              {p.name}
                              <Show when={p.hash === active()}>
                                {" "}<span class="text-gray-400">(active)</span>
                              </Show>
                              {" — "}<span class="font-mono">{p.hash.slice(0, 10)}…</span>
                            </li>
                          )}
                        </For>
                      </ul>
                    </Show>
                  </div>
                )}
              </For>
            </div>
          </div>
        </div>
      </Show>
    </Card>
  );
}