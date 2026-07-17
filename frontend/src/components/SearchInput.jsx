import { createSignal, onCleanup } from "solid-js";

// SearchInput is a debounced text input that emits onSearch with
// the trimmed query after a short idle period (default 300ms). The
// debounce keeps the API from being hammered on every keystroke;
// the parent treats onSearch as the source of truth and resets
// the page offset to 0 when it fires.
//
// The input is uncontrolled-by-value: it keeps its own internal
// signal so the debounced emit doesn't make the cursor jump while
// the user is still typing. onSearch fires only on settled input.
export default function SearchInput(props) {
  const placeholder = props.placeholder || "Search...";
  const debounceMs = props.debounceMs ?? 300;
  const [value, setValue] = createSignal(props.initial || "");
  let timer = null;

  const onChange = (e) => {
    const v = e.target.value;
    setValue(v);
    if (timer) clearTimeout(timer);
    timer = setTimeout(() => {
      props.onSearch?.(v.trim());
    }, debounceMs);
  };

  onCleanup(() => {
    if (timer) clearTimeout(timer);
  });

  return (
    <input
      type="search"
      placeholder={placeholder}
      value={value()}
      onInput={onChange}
      class="text-sm border border-border rounded bg-surface text-text-base placeholder-text-muted px-3 py-1 w-full sm:w-64 focus:outline-none focus:ring-2 focus:ring-primary"
    />
  );
}