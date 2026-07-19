import { For, Show } from "solid-js";
import Badge from "../../components/Badge";
import Button from "../../components/Button";
import Card from "../../components/Card";
import { ROLE_BADGE } from "./constants";

/**
 * Table of users with their roles and per-role "Remove" action.
 *
 * Props:
 *   - users:         accessor () => Array<User>
 *   - onRemoveRole:  (userId, role, repoId) => void | Promise<void>
 */
export default function UsersTable(props) {
  const users = () => props.users() || [];

  return (
    <Card>
      <h2 class="text-lg font-semibold mb-4 dark:text-white">Users</h2>
      <div class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead>
            <tr class="text-left border-b dark:border-gray-700">
              <th class="py-3 px-4 font-medium text-gray-600 dark:text-gray-400">User</th>
              <th class="py-3 px-4 font-medium text-gray-600 dark:text-gray-400">Email</th>
              <th class="py-3 px-4 font-medium text-gray-600 dark:text-gray-400">Roles</th>
              <th class="py-3 px-4 font-medium text-gray-600 dark:text-gray-400">Actions</th>
            </tr>
          </thead>
          <tbody>
            <For each={users()}>
              {(user) => (
                <tr class="border-b dark:border-gray-700 hover:bg-gray-50 dark:hover:bg-gray-700/50">
                  <td class="py-3 px-4">
                    <div class="font-medium dark:text-gray-200">
                      {user.display_name || "\u2014"}
                    </div>
                    <div class="text-xs text-gray-400 dark:text-gray-500 font-mono">{user.id}</div>
                  </td>
                  <td class="py-3 px-4 text-gray-600 dark:text-gray-400">{user.email}</td>
                  <td class="py-3 px-4">
                    <div class="flex flex-wrap gap-1">
                      <Show
                        when={user.roles && user.roles.length > 0}
                        fallback={
                          <span class="text-xs text-gray-400 dark:text-gray-500">No roles</span>
                        }
                      >
                        <For each={user.roles}>
                          {(role) => (
                            <Badge variant={ROLE_BADGE[role.role] || "gray"}>
                              {role.role}
                              <Show when={role.repository_id && role.repository_id !== "*"}>
                                <span class="opacity-60 ml-1">@{role.repository_id}</span>
                              </Show>
                            </Badge>
                          )}
                        </For>
                      </Show>
                    </div>
                  </td>
                  <td class="py-3 px-4">
                    <Show when={user.roles && user.roles.length > 0}>
                      <div class="flex flex-wrap gap-1">
                        <For each={user.roles}>
                          {(role) => (
                            <Button
                              variant="ghost"
                              onClick={() =>
                                props.onRemoveRole?.(user.id, role.role, role.repository_id || "*")
                              }
                              class="text-xs px-0 py-0"
                            >
                              Remove
                            </Button>
                          )}
                        </For>
                      </div>
                    </Show>
                  </td>
                </tr>
              )}
            </For>
          </tbody>
        </table>
      </div>
    </Card>
  );
}
