import { createSignal, For, Show } from "solid-js";
import Badge from "../../components/Badge";
import Button from "../../components/Button";
import Card from "../../components/Card";
import { api } from "../../services/api";

/**
 * HostSkipProviders renders the active learned + manual
 * (host, provider) skip list for the active repository, with
 * controls to manually add/remove/clear skips. A manual skip
 * pins a tier out for a host indefinitely; a learned skip is
 * auto-added by the strategy when the failure rate crosses the
 * threshold and auto-cleared when the rate drops back below it
 * after a success. Operators use this card to pin out a tier
 * the auto-skip hasn't caught yet, or to force-retry a tier
 * the auto-skip learned to skip.
 *
 * Props:
 *   skips: the host_skip_providers array from /sources/providers
 *   onChanged: () => Promise<void> callback to refetch after a
 *     mutation (the parent owns the providers resource).
 */
export default function HostSkipProviders(props) {
  const [host, setHost] = createSignal("");
  const [providerId, setProviderId] = createSignal("fetch");
  const [busy, setBusy] = createSignal(false);
  const [err, setErr] = createSignal("");

  // skips may be passed as a value (the array) or as an
  // accessor function (() => array). Unwrap either form.
  const skips = () => {
    const v = props.skips;
    if (typeof v === "function") return v() || [];
    return v || [];
  };

  const mutate = async (fn) => {
    setBusy(true);
    setErr("");
    try {
      await fn();
      if (props.onChanged) await props.onChanged();
    } catch (e) {
      setErr(e.message || "request failed");
    } finally {
      setBusy(false);
    }
  };

  const add = () => {
    if (!host() || !providerId()) return;
    mutate(() => api.addHostSkip(host().trim(), providerId().trim()));
  };

  const remove = (h, pid) => mutate(() => api.removeHostSkip(h, pid));

  const clearAll = () => mutate(() => api.clearHostSkips());

  return (
    <Card class="mb-6">
      <div class="flex items-center gap-2 mb-1">
        <h2 class="text-lg font-semibold dark:text-white">Host skip list</h2>
        <Badge variant="green">active</Badge>
      </div>
      <p class="text-sm text-gray-500 dark:text-gray-400 mb-4">
        Tiers the strategy skips for a host. Learned rows are auto-added when the failure rate
        crosses the threshold and auto-cleared after a success; manual rows stay until removed.
      </p>

      <Show when={err()}>
        <p class="text-sm text-red-600 dark:text-red-400 mb-3">{err()}</p>
      </Show>

      <div class="flex flex-wrap items-end gap-2 mb-4">
        <label class="block">
          <span class="block text-xs font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wide mb-1">
            Host
          </span>
          <input
            type="text"
            value={host()}
            onInput={(e) => setHost(e.currentTarget.value)}
            placeholder="e.g. reddit.com"
            class="border dark:border-gray-700 dark:bg-gray-800 rounded px-2 py-1 text-sm w-48"
          />
        </label>
        <label class="block">
          <span class="block text-xs font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wide mb-1">
            Provider
          </span>
          <select
            value={providerId()}
            onChange={(e) => setProviderId(e.currentTarget.value)}
            class="border dark:border-gray-700 dark:bg-gray-800 rounded px-2 py-1 text-sm"
          >
            <option value="fetch">fetch</option>
            <option value="tls">tls</option>
            <option value="unpaywall">unpaywall</option>
            <option value="flaresolverr">flaresolverr</option>
          </select>
        </label>
        <Button onClick={add} disabled={busy() || !host()}>
          Add skip
        </Button>
        <Show when={skips().length > 0}>
          <Button onClick={clearAll} disabled={busy()} variant="danger">
            Clear all
          </Button>
        </Show>
      </div>

      <Show
        when={skips().length > 0}
        fallback={
          <p class="text-sm text-gray-400">
            No active skips. The strategy is trying every configured tier for every host.
          </p>
        }
      >
        <div class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="text-left text-xs font-semibold text-gray-500 dark:text-gray-400 uppercase tracking-wide border-b dark:border-gray-700">
                <th class="py-2 pr-4">Host</th>
                <th class="py-2 pr-4">Provider</th>
                <th class="py-2 pr-4">Rate</th>
                <th class="py-2 pr-4">Sample</th>
                <th class="py-2 pr-4">Expires</th>
                <th class="py-2 pr-4">Type</th>
                <th class="py-2"> </th>
              </tr>
            </thead>
            <tbody>
              <For each={skips()}>
                {(s) => (
                  <tr class="border-b dark:border-gray-700 last:border-0">
                    <td class="py-2 pr-4 font-mono text-xs text-gray-700 dark:text-gray-300">
                      {s.host}
                    </td>
                    <td class="py-2 pr-4 font-mono text-xs text-gray-700 dark:text-gray-300">
                      {s.provider_id}
                    </td>
                    <td class="py-2 pr-4 text-gray-600 dark:text-gray-400">
                      {(s.failure_rate * 100).toFixed(0)}%
                    </td>
                    <td class="py-2 pr-4 text-gray-600 dark:text-gray-400">{s.sample_size}</td>
                    <td class="py-2 pr-4 text-gray-600 dark:text-gray-400">{s.expires_at}</td>
                    <td class="py-2 pr-4">
                      <Show when={s.manual} fallback={<Badge variant="yellow">learned</Badge>}>
                        <Badge variant="red">manual</Badge>
                      </Show>
                    </td>
                    <td class="py-2">
                      <Button onClick={() => remove(s.host, s.provider_id)} disabled={busy()}>
                        Remove
                      </Button>
                    </td>
                  </tr>
                )}
              </For>
            </tbody>
          </table>
        </div>
      </Show>
    </Card>
  );
}
