import { Show } from "solid-js";

export default function EmptyState(props) {
  return (
    <div class={`bg-surface border border-border rounded-lg shadow-card dark:shadow-card-dark p-8 text-center ${props.class || ""}`}>
      <p class="text-text-muted text-sm">{props.title}</p>
      <Show when={props.description}>
        <p class="text-text-muted text-xs mt-1">{props.description}</p>
      </Show>
    </div>
  );
}