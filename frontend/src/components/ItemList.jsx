import { Show, For } from "solid-js";
import Card from "./Card";
import Badge from "./Badge";

/**
 * Titled card containing a list of items, each rendered with a name/id
 * and a right-hand slot.
 *
 * Generic enough to be reused for provider lists, plugin lists, etc.
 *
 * Props:
 *   - title:       string
 *   - description: string
 *   - items:       Array<{ name, id, type?, ... }>
 *   - emptyText:   string shown when items is empty
 *   - getKey:      optional (item) => key (defaults to item.id)
 *   - children:    optional render-prop (item) => JSX shown on the right
 *                  of each row. If omitted, a green Badge with item.type is shown.
 */
export default function ItemList(props) {
  const renderRight = (item) =>
    props.children
      ? props.children(item)
      : <Badge variant="green">{item.type}</Badge>;

  return (
    <Card class="mb-6">
      <h2 class="text-lg font-semibold mb-1 text-text-base">{props.title}</h2>
      <Show when={props.description}>
        <p class="text-sm text-text-muted mb-4">{props.description}</p>
      </Show>
      <div class="space-y-2">
        <For each={props.items}>
          {(item) => (
            <div class="flex items-center justify-between border border-border rounded p-3">
              <div>
                <span class="font-medium text-sm text-text-base">{item.name}</span>
                <span class="ml-2 text-xs text-text-muted font-mono">{item.id}</span>
              </div>
              {renderRight(item)}
            </div>
          )}
        </For>
        <Show when={props.items && props.items.length === 0}>
          <p class="text-sm text-text-muted">{props.emptyText}</p>
        </Show>
      </div>
    </Card>
  );
}
