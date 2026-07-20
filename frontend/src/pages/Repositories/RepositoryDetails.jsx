import { createResource, For, Show } from "solid-js";
import Card from "../../components/Card";
import { api } from "../../services/api";
import { useRepository } from "../../store/repository";

/**
 * A small card that, when a repository is selected, lists the
 * permissions the current user has on it. Demonstrates the
 * `useRepository` hook being the single source of truth for the
 * current scope: changing the active repository from the Layout's
 * selector will refresh this card automatically because it reads
 * `repo.currentRepo()` reactively.
 */
export default function RepositoryDetails() {
  const repo = useRepository();

  // Re-runs whenever the current repository changes, because the
  // resource source is the current repository id.
  const [perms] = createResource(
    () => (repo.currentRepo() ? repo.currentRepo().id : ""),
    (id) => (id ? api.getRepositoryPermissions(id) : null),
  );

  return (
    <Show when={repo.currentRepo()}>
      <Card>
        <h2 class="text-lg font-semibold mb-2 dark:text-white">
          Your permissions on {repo.currentRepo().name}
        </h2>
        <p class="text-xs text-gray-500 dark:text-gray-400 mb-3 font-mono">
          {repo.currentRepo().id}
        </p>

        <Show
          when={perms() && !perms().loading}
          fallback={<p class="text-sm text-gray-500 dark:text-gray-400">Loading permissions...</p>}
        >
          <Show
            when={perms().system_admin}
            fallback={
              <div class="flex flex-wrap gap-2">
                <For each={perms().permissions || []}>
                  {(p) => (
                    <span class="text-xs font-mono px-2 py-0.5 rounded bg-gray-100 dark:bg-gray-700 dark:text-gray-300">
                      {p.resource}:{p.action}
                    </span>
                  )}
                </For>
              </div>
            }
          >
            <p class="text-sm text-purple-700 dark:text-purple-300">
              System administrator — full access on every repository.
            </p>
          </Show>
        </Show>
      </Card>
    </Show>
  );
}
