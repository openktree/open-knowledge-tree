import { For } from "solid-js";
import Badge from "../../components/Badge";
import Card from "../../components/Card";

/**
 * Card listing the available permissions (resource:action badges).
 *
 * Props:
 *   - permissions: accessor () => Array<{ resource, action }>
 */
export default function AvailablePermissions(props) {
  const perms = () => props.permissions() || [];

  return (
    <Card>
      <h2 class="text-lg font-semibold mb-4 dark:text-white">Available Permissions</h2>
      <div class="flex flex-wrap gap-2">
        <For each={perms()}>
          {(perm) => (
            <Badge variant="gray">
              {perm.resource}:{perm.action}
            </Badge>
          )}
        </For>
      </div>
    </Card>
  );
}
