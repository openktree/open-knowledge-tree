import { createMemo, createSignal, For, Show } from "solid-js";
import Badge from "../../components/Badge";
import Button from "../../components/Button";
import Card from "../../components/Card";
import { api } from "../../services/api";

const PAGE_SIZE = 15;

// Chain order (weakest → strongest). A "chain entry point" of
// flaresolverr means skip every tier weaker than flaresolverr
// (unpaywall, tls, fetch). "Do not pull" skips all four.
const CHAIN = ["unpaywall", "tls", "fetch", "flaresolverr"];

// skipChoiceForHost derives the current dropdown selection from
// the active skip list for a host. Returns the provider id that
// is the strongest NON-skipped tier (the effective entry point),
// or "ban" when every fetch tier is skipped, or "" when nothing
// is skipped.
function skipChoiceForHost(host, skips) {
  const skipped = new Set(
    skips.filter((s) => s.host === host && s.manual).map((s) => s.provider_id),
  );
  if (CHAIN.every((p) => skipped.has(p))) return "ban";
  for (const p of CHAIN) {
    if (!skipped.has(p)) return p;
  }
  return "";
}

// providerIdsToSkip returns the list of tiers to skip for a
// given choice. "ban" skips all; a provider id skips every
// tier weaker than it (the chosen one stays as the entry
// point); "" skips none.
function providerIdsToSkip(choice) {
  if (choice === "ban") return [...CHAIN];
  if (choice === "" || !CHAIN.includes(choice)) return [];
  const idx = CHAIN.indexOf(choice);
  return CHAIN.slice(0, idx);
}

/**
 * HostFailureMatrix renders the merged per-host fetch-diagnostics
 * table with per-provider failure/success columns, client-side
 * pagination, and a per-row dropdown to set the chain entry
 * point (skip weaker tiers) or "do not pull" (ban all tiers).
 *
 * Props:
 *   matrix: accessor-or-value, the host_failure_matrix array
 *     from /sources/providers.
 *   skips: accessor-or-value, the host_skip_providers array.
 *   onChanged: () => Promise<void> refetch after a mutation.
 */
export default function HostFailureMatrix(props) {
  const matrix = () => {
    const v = props.matrix;
    if (typeof v === "function") return v() || [];
    return v || [];
  };
  const skips = () => {
    const v = props.skips;
    if (typeof v === "function") return v() || [];
    return v || [];
  };

  const [page, setPage] = createSignal(0);
  const [busyHost, setBusyHost] = createSignal("");
  const [err, setErr] = createSignal("");

  const pageCount = createMemo(() => Math.max(1, Math.ceil(matrix().length / PAGE_SIZE)));
  const pageRows = createMemo(() => {
    const start = page() * PAGE_SIZE;
    return matrix().slice(start, start + PAGE_SIZE);
  });

  const choiceFor = (host) => skipChoiceForHost(host, skips());

  const apply = async (host, choice) => {
    setBusyHost(host);
    setErr("");
    try {
      await api.setHostSkip(host, providerIdsToSkip(choice));
      if (props.onChanged) await props.onChanged();
    } catch (e) {
      setErr(e.message || "request failed");
    } finally {
      setBusyHost("");
    }
  };

  const fmt = (fail, succ) => {
    if (fail === 0 && succ === 0) return <span class="text-gray-300 dark:text-gray-600">—</span>;
    return (
      <span>
        <span class="text-red-600 dark:text-red-400">{fail}</span>
        <span class="text-gray-400 dark:text-gray-500"> / </span>
        <span class="text-gray-600 dark:text-gray-400">{succ}</span>
      </span>
    );
  };

  return (
    <Card class="mb-6">
      <div class="flex items-center gap-2 mb-1">
        <h2 class="text-lg font-semibold dark:text-white">Host fetch diagnostics</h2>
        <Badge variant="yellow">advisory</Badge>
      </div>
      <p class="text-sm text-gray-500 dark:text-gray-400 mb-4">
        Per-host failure/success counts by provider tier. Use the row dropdown to set the chain
        entry point (skip weaker tiers) or ban the host entirely ("do not pull"). Changes apply to
        the active repository.
      </p>

      <Show when={err()}>
        <p class="text-sm text-red-600 dark:text-red-400 mb-3">{err()}</p>
      </Show>

      <Show
        when={matrix().length > 0}
        fallback={
          <p class="text-sm text-gray-400">
            No fetch failures recorded yet. Select a repository with <code>fetch_attempts</code>{" "}
            data to populate this table.
          </p>
        }
      >
        <div class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="text-left text-xs font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wide border-b dark:border-gray-700">
                <th class="py-2 pr-4">Host</th>
                <th class="py-2 pr-4">Total</th>
                <th class="py-2 pr-4">unpaywall</th>
                <th class="py-2 pr-4">tls</th>
                <th class="py-2 pr-4">fetch</th>
                <th class="py-2 pr-4">flaresolverr</th>
                <th class="py-2 pr-4">Skip config</th>
              </tr>
            </thead>
            <tbody>
              <For each={pageRows()}>
                {(row) => (
                  <tr class="border-b dark:border-gray-700 last:border-0">
                    <td class="py-2 pr-4 font-mono text-xs text-gray-700 dark:text-gray-300">
                      {row.host}
                    </td>
                    <td class="py-2 pr-4 text-gray-600 dark:text-gray-400">{row.total_attempts}</td>
                    <td class="py-2 pr-4 text-xs">
                      {fmt(row.unpaywall_failures, row.unpaywall_successes)}
                    </td>
                    <td class="py-2 pr-4 text-xs">{fmt(row.tls_failures, row.tls_successes)}</td>
                    <td class="py-2 pr-4 text-xs">
                      {fmt(row.fetch_failures, row.fetch_successes)}
                    </td>
                    <td class="py-2 pr-4 text-xs">
                      {fmt(row.flaresolverr_failures, row.flaresolverr_successes)}
                    </td>
                    <td class="py-2 pr-4">
                      <select
                        value={choiceFor(row.host)}
                        disabled={busyHost() === row.host}
                        onChange={(e) => apply(row.host, e.currentTarget.value)}
                        class="border dark:border-gray-700 dark:bg-gray-800 rounded px-2 py-1 text-xs"
                      >
                        <option value="">use full chain</option>
                        <option value="unpaywall">start at unpaywall</option>
                        <option value="tls">start at tls</option>
                        <option value="fetch">start at fetch</option>
                        <option value="flaresolverr">start at flaresolverr</option>
                        <option value="ban">do not pull</option>
                      </select>
                    </td>
                  </tr>
                )}
              </For>
            </tbody>
          </table>
        </div>

        <Show when={pageCount() > 1}>
          <div class="flex items-center justify-between mt-4">
            <span class="text-xs text-gray-500 dark:text-gray-400">
              Page {page() + 1} of {pageCount()} ({matrix().length} hosts)
            </span>
            <div class="flex gap-2">
              <Button
                variant="secondary"
                onClick={() => setPage((p) => Math.max(0, p - 1))}
                disabled={page() === 0}
              >
                Prev
              </Button>
              <Button
                variant="secondary"
                onClick={() => setPage((p) => Math.min(pageCount() - 1, p + 1))}
                disabled={page() >= pageCount() - 1}
              >
                Next
              </Button>
            </div>
          </div>
        </Show>
      </Show>
    </Card>
  );
}
