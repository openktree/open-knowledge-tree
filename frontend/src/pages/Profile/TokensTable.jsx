import { For, Show } from "solid-js";
import Badge from "../../components/Badge";
import Button from "../../components/Button";
import Card from "../../components/Card";
import EmptyState from "../../components/EmptyState";
import { formatDate } from "./constants";

/**
 * TokensTable — the "API Tokens" tab. Lists the caller's personal
 * access tokens with their recognizable prefix, scope, repo
 * restriction, expiry, last-used, and a per-row "Revoke" action.
 * Revoked keys are shown greyed out.
 *
 * Props:
 *   - keys:         accessor () => Array<Key> | null
 *   - repositories: accessor () => Array<{id, name}> | null  — used
 *                  to render the repo name instead of the UUID in
 *                  the Repository column. Unknown IDs fall back to
 *                  the raw UUID (e.g. a repo the user can no longer
 *                  see, or a deleted repo).
 *   - onRevoke:    (keyID) => void | Promise<void>
 *   - onAlert:     (alert) => void
 */
export default function TokensTable(props) {
  const keys = () => props.keys() || [];

  // repoNameByID looks up a repository's name from the
  // repositories accessor. Returns the name when found, the raw
  // UUID when not (so the user still has a stable identifier for a
  // repo they lost access to or that was deleted), and "All repos"
  // when repository_id is null/empty.
  const repoNameByID = (id) => {
    if (!id) return "All repos";
    const repos = props.repositories?.() || [];
    const match = repos.find((r) => r.id === id);
    return match ? match.name : id;
  };

  return (
    <Card>
      <div class="flex items-center justify-between mb-4">
        <h2 class="text-lg font-semibold dark:text-white">API Tokens</h2>
        <span class="text-xs text-text-muted">{keys().length} key(s)</span>
      </div>

      <Show
        when={keys().length > 0}
        fallback={
          <EmptyState
            title="No API tokens yet"
            description="Create a token to authenticate scripts, CI, or the CLI against the OKT REST API."
          />
        }
      >
        <div class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="text-left border-b dark:border-gray-700">
                <th class="py-3 px-4 font-medium text-gray-600 dark:text-gray-400">Name</th>
                <th class="py-3 px-4 font-medium text-gray-600 dark:text-gray-400">Prefix</th>
                <th class="py-3 px-4 font-medium text-gray-600 dark:text-gray-400">Permissions</th>
                <th class="py-3 px-4 font-medium text-gray-600 dark:text-gray-400">Repository</th>
                <th class="py-3 px-4 font-medium text-gray-600 dark:text-gray-400">Last used</th>
                <th class="py-3 px-4 font-medium text-gray-600 dark:text-gray-400">Expires</th>
                <th class="py-3 px-4 font-medium text-gray-600 dark:text-gray-400">Actions</th>
              </tr>
            </thead>
            <tbody>
              <For each={keys()}>
                {(key) => {
                  const revoked = () => !!key.revoked_at;
                  return (
                    <tr
                      class="border-b dark:border-gray-700 hover:bg-gray-50 dark:hover:bg-gray-700/50"
                      classList={{ "opacity-50": revoked() }}
                    >
                      <td class="py-3 px-4">
                        <div class="font-medium dark:text-gray-200">{key.name}</div>
                        <Show when={revoked()}>
                          <Badge variant="gray">revoked</Badge>
                        </Show>
                      </td>
                      <td class="py-3 px-4 font-mono text-xs text-text-muted">{key.prefix}</td>
                      <td class="py-3 px-4">
                        <div class="flex flex-wrap gap-1">
                          <Show
                            when={key.permissions && key.permissions.length > 0}
                            fallback={<span class="text-xs text-text-muted">No scope</span>}
                          >
                            <For each={key.permissions}>
                              {(perm) => <Badge variant="blue">{perm}</Badge>}
                            </For>
                          </Show>
                        </div>
                      </td>
                      <td class="py-3 px-4 text-xs text-text-muted">
                        <Show
                          when={key.repository_id}
                          fallback={<span class="text-text-muted">All repos</span>}
                        >
                          <span class="text-text-base">{repoNameByID(key.repository_id)}</span>
                        </Show>
                      </td>
                      <td class="py-3 px-4 text-xs text-text-muted">
                        {formatDate(key.last_used_at)}
                      </td>
                      <td class="py-3 px-4 text-xs text-text-muted">
                        {formatDate(key.expires_at)}
                      </td>
                      <td class="py-3 px-4">
                        <Show when={!revoked()}>
                          <Button
                            variant="ghost"
                            class="text-xs px-0 py-0"
                            onClick={() => props.onRevoke?.(key.id)}
                          >
                            Revoke
                          </Button>
                        </Show>
                      </td>
                    </tr>
                  );
                }}
              </For>
            </tbody>
          </table>
        </div>
      </Show>
    </Card>
  );
}
