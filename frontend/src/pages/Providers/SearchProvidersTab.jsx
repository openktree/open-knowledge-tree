import { For, Show } from "solid-js";
import Badge from "../../components/Badge";
import ItemList from "../../components/ItemList";
import TestSearchPanel from "./TestSearchPanel";

export default function SearchProvidersTab(props) {
  const providers = () => props.providers() || { search: [], resolution: [] };

  return (
    <div>
      <ItemList
        title="Search Providers"
        description="Available search backends. Use the Test Search panel below to try one against a live query."
        items={providers().search}
        emptyText="No search providers configured."
      >
        {(p) => (
          <div class="flex items-center gap-2">
            <Show when={p.enabled_for_repo === false}>
              <Badge variant="gray">Disabled for this repo</Badge>
            </Show>
            <For each={p.supports || []}>
              {(s) => <Badge variant="blue">{s}</Badge>}
            </For>
          </div>
        )}
      </ItemList>

      <TestSearchPanel providers={providers} />
    </div>
  );
}
