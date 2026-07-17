import { For, Show } from "solid-js";
import Badge from "../../components/Badge";
import Card from "../../components/Card";

/**
 * FlareSkipCandidates renders the per-host FlareSolverr
 * failure/success counts the backend surfaces on
 * /sources/providers under `flare_skip_candidates`. The card
 * is purely informational: the strategy does NOT enforce a
 * skip list yet. Operators review the list and, when a host
 * has many failures and zero successes, it is a candidate for
 * the future host_skip_providers config key.
 *
 * A row is "strong" (highlighted) when flare_successes = 0 and
 * flare_failures >= 3 — those are the hosts where FlareSolverr
 * has never succeeded and is just burning the 60s per-provider
 * timeout on every fetch.
 */
export default function FlareSkipCandidates(props) {
  const candidates = () => props.candidates || [];

  const isStrong = (c) =>
    c.flare_successes === 0 && c.flare_failures >= 3;

  return (
    <Show when={candidates().length > 0}>
      <Card class="mb-6">
        <div class="flex items-center gap-2 mb-1">
          <h2 class="text-lg font-semibold dark:text-white">
            FlareSolverr skip-list candidates
          </h2>
          <Badge variant="yellow">advisory</Badge>
        </div>
        <p class="text-sm text-gray-500 dark:text-gray-400 mb-4">
          Hosts where FlareSolverr was tried at least once. A host
          with failures and zero successes is a candidate to pin
          out of the FlareSolverr tier (the strategy does not
          enforce a skip list yet). Surfacing the data now so the
          blacklist is ready to wire.
        </p>
        <div class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="text-left text-xs font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wide border-b dark:border-gray-700">
                <th class="py-2 pr-4">Host</th>
                <th class="py-2 pr-4">Total</th>
                <th class="py-2 pr-4">Flare failures</th>
                <th class="py-2 pr-4">Flare successes</th>
                <th class="py-2">Signal</th>
              </tr>
            </thead>
            <tbody>
              <For each={candidates()}>
                {(c) => (
                  <tr class="border-b dark:border-gray-700 last:border-0">
                    <td class="py-2 pr-4 font-mono text-xs text-gray-700 dark:text-gray-300">
                      {c.host}
                    </td>
                    <td class="py-2 pr-4 text-gray-600 dark:text-gray-400">
                      {c.total_attempts}
                    </td>
                    <td class="py-2 pr-4 text-red-600 dark:text-red-400">
                      {c.flare_failures}
                    </td>
                    <td class="py-2 pr-4 text-gray-600 dark:text-gray-400">
                      {c.flare_successes}
                    </td>
                    <td class="py-2">
                      <Show
                        when={isStrong(c)}
                        fallback={<span class="text-gray-400 text-xs">mixed</span>}
                      >
                        <Badge variant="red">strong skip candidate</Badge>
                      </Show>
                    </td>
                  </tr>
                )}
              </For>
            </tbody>
          </table>
        </div>
      </Card>
    </Show>
  );
}