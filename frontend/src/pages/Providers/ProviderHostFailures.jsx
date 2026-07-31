import { For, Show } from "solid-js";
import Badge from "../../components/Badge";
import Card from "../../components/Card";

/**
 * ProviderHostFailures renders one "hosts that don't reply"
 * card per resolution provider, from the
 * `host_failures_by_provider` map the backend surfaces on
 * /sources/providers. Each card lists the hosts where that
 * provider was tried, with failure/success counts. A host with
 * failures > 0 and successes = 0 is highlighted as a "strong
 * skip candidate" — the operator can later pin it out of that
 * tier once host_skip_providers is wired.
 *
 * Purely informational: the strategy does NOT enforce a skip
 * list yet. Surfacing the data per provider so the blacklist
 * is ready to wire.
 *
 * Props:
 *   byProvider: the host_failures_by_provider map
 *     { [providerId]: [{host, total_attempts, failures, successes}] }
 */
export default function ProviderHostFailures(props) {
  // byProvider may be passed as a value (the map) or as an
  // accessor function (() => map). Unwrap either form so the
  // card works regardless of the caller's convention.
  const byProvider = () => {
    const v = props.byProvider;
    if (typeof v === "function") return v() || {};
    return v || {};
  };
  const entries = () => {
    const map = byProvider();
    return Object.entries(map).map(([pid, hosts]) => ({
      providerId: pid,
      hosts: hosts || [],
    }));
  };

  const isStrong = (h) => h.successes === 0 && h.failures >= 3;

  return (
    <Show
      when={entries().length > 0}
      fallback={
        <Card class="mb-6">
          <div class="flex items-center gap-2 mb-1">
            <h2 class="text-lg font-semibold dark:text-white">Host failures by provider</h2>
            <Badge variant="yellow">advisory</Badge>
          </div>
          <p class="text-sm text-gray-500 dark:text-gray-400">
            No fetch failures recorded yet. This card lists hosts where each provider tier was
            tried, with failure/success counts, once a repository has <code>fetch_attempts</code>
            data. Select a repository to see per-host diagnostics.
          </p>
        </Card>
      }
    >
      <For each={entries()}>
        {(entry) => (
          <Card class="mb-6">
            <div class="flex items-center gap-2 mb-1">
              <h2 class="text-lg font-semibold dark:text-white">
                {providerLabel(entry.providerId)} — hosts that don't reply
              </h2>
              <Badge variant="yellow">advisory</Badge>
            </div>
            <p class="text-sm text-gray-500 dark:text-gray-400 mb-4">
              Hosts where{" "}
              <code class="text-xs bg-gray-100 dark:bg-gray-800 px-1 py-0.5 rounded">
                {entry.providerId}
              </code>{" "}
              was tried at least once. A host with failures and zero successes is a candidate to pin
              out of this tier.
            </p>
            <Show when={entry.hosts.length > 0}>
              <div class="overflow-x-auto">
                <table class="w-full text-sm">
                  <thead>
                    <tr class="text-left text-xs font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wide border-b dark:border-gray-700">
                      <th class="py-2 pr-4">Host</th>
                      <th class="py-2 pr-4">Total</th>
                      <th class="py-2 pr-4">Failures</th>
                      <th class="py-2 pr-4">Successes</th>
                      <th class="py-2">Signal</th>
                    </tr>
                  </thead>
                  <tbody>
                    <For each={entry.hosts}>
                      {(h) => (
                        <tr class="border-b dark:border-gray-700 last:border-0">
                          <td class="py-2 pr-4 font-mono text-xs text-gray-700 dark:text-gray-300">
                            {h.host}
                          </td>
                          <td class="py-2 pr-4 text-gray-600 dark:text-gray-400">
                            {h.total_attempts}
                          </td>
                          <td class="py-2 pr-4 text-red-600 dark:text-red-400">{h.failures}</td>
                          <td class="py-2 pr-4 text-gray-600 dark:text-gray-400">{h.successes}</td>
                          <td class="py-2">
                            <Show
                              when={isStrong(h)}
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
            </Show>
          </Card>
        )}
      </For>
    </Show>
  );
}

const providerLabel = (pid) => {
  switch (pid) {
    case "fetch":
      return "HTTP Fetch";
    case "tls":
      return "TLS Impersonation";
    case "unpaywall":
      return "Unpaywall";
    case "flaresolverr":
      return "FlareSolverr";
    case "url_safety":
      return "URL Safety (SSRF guard)";
    default:
      return pid;
  }
};
