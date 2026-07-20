import { Show } from "solid-js";

const variantClasses = {
  error: "bg-danger/10 text-danger",
  success: "bg-success/10 text-success",
  warning: "bg-warning/10 text-warning",
  info: "bg-info/10 text-info",
};

export default function Alert(props) {
  return (
    <Show when={props.message}>
      <div
        class={`p-4 rounded text-sm ${variantClasses[props.variant] || variantClasses.info} ${props.class || ""}`}
      >
        {props.message}
        <Show when={props.onDismiss}>
          <button onClick={props.onDismiss} class="ml-2 font-bold">
            &times;
          </button>
        </Show>
      </div>
    </Show>
  );
}
