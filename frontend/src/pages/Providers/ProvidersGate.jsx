import { Show } from "solid-js";
import EmptyState from "../../components/EmptyState";

export default function ProvidersGate(props) {
  const label = props.permission || "providers";
  return (
    <Show
      when={props.can}
      fallback={<EmptyState title={`You do not have permission to view ${label}.`} />}
    >
      <Show when={props.loaded()} fallback={<EmptyState title={`Loading ${label}...`} />}>
        {props.children}
      </Show>
    </Show>
  );
}
