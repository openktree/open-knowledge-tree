import { Show } from "solid-js";

export default function FormField(props) {
  return (
    <div class={props.class}>
      <Show when={props.label}>
        <label
          for={props.name}
          class={`block mb-1 text-sm font-medium text-text-base ${props.labelClass || ""}`}
        >
          {props.label}
        </label>
      </Show>
      <Show
        when={props.type === "select"}
        fallback={
          <input
            id={props.name}
            type={props.type || "text"}
            value={props.value}
            onInput={(e) => props.onChange?.(e.target.value)}
            placeholder={props.placeholder}
            required={props.required}
            disabled={props.disabled}
            class={`w-full px-3 py-2 border rounded bg-surface text-text-base placeholder-text-muted focus:outline-none focus:ring-2 focus:ring-primary ${props.error ? "border-danger" : "border-border"} ${props.inputClass || ""}`}
          />
        }
      >
        <select
          id={props.name}
          value={props.value}
          onChange={(e) => props.onChange?.(e.target.value)}
          class={`border border-border rounded px-3 py-2 text-sm bg-surface text-text-base ${props.inputClass || ""}`}
        >
          {props.children}
        </select>
      </Show>
      <Show when={props.error}>
        <p class="text-danger text-xs mt-1">{props.error}</p>
      </Show>
    </div>
  );
}