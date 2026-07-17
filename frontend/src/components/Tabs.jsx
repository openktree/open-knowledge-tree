import { For } from "solid-js";

export default function Tabs(props) {
  return (
    <nav class={`flex border-b border-border mb-6 ${props.class || ""}`}>
      <For each={props.tabs}>
        {(tab) => (
          <button
            onClick={() => props.onChange(tab.value)}
            class={`px-4 py-2 text-sm font-medium border-b-2 -mb-px transition ${
              props.active === tab.value
                ? "border-primary text-primary-fg"
                : "border-transparent text-text-muted hover:text-text-base"
            }`}
          >
            {tab.label}
          </button>
        )}
      </For>
    </nav>
  );
}