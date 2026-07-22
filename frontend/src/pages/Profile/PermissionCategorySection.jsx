import { createSignal, For, Show } from "solid-js";

/**
 * PermissionCategorySection — one collapsible category in the
 * create-token modal's permission picker. Renders a header row
 * (label + chevron + description + the count of selected perms in
 * that category) and, when expanded, a checkbox list of the
 * category's options. Mirrors GitHub fine-grained PATs: each
 * resource category is its own collapsible so the user can focus on
 * the scope they care about instead of scanning a 14-row flat list.
 *
 * Props:
 *   - category:    {label, description, options: [{value, label}]}
 *   - selected:    accessor () => Set<string>   — the modal's selectedPerms
 *   - onToggle:    (value) => void              — togglePerm
 *   - defaultOpen: boolean (default false; the "All permissions"
 *                  category passes true so the admin shortcut is
 *                  visible without expanding)
 */
export default function PermissionCategorySection(props) {
  const [open, setOpen] = createSignal(props.defaultOpen ?? false);
  const category = () => props.category;
  const selected = () => props.selected() || new Set();

  const selectedCount = () => {
    const s = selected();
    return category().options.filter((o) => s.has(o.value)).length;
  };

  return (
    <div class="border border-border rounded">
      <button
        type="button"
        class="w-full text-left px-3 py-2 flex items-center gap-2 hover:bg-primary-soft transition"
        onClick={() => setOpen((o) => !o)}
        aria-expanded={open()}
      >
        <span class="text-text-muted text-xs w-3 inline-block">{open() ? "\u25bc" : "\u25b6"}</span>
        <span class="text-sm font-medium text-text-base flex-1">{category().label}</span>
        <Show when={selectedCount() > 0}>
          <span class="text-xs text-primary-fg bg-primary-soft px-1.5 py-0.5 rounded">
            {selectedCount()}
          </span>
        </Show>
      </button>
      <Show when={open()}>
        <div class="px-3 pb-3 pt-1 border-t border-border">
          <Show when={category().description}>
            <p class="text-xs text-text-muted mb-2 mt-2">{category().description}</p>
          </Show>
          <div class="grid grid-cols-2 gap-1">
            <For each={category().options}>
              {(opt) => (
                <label class="flex items-center gap-2 text-sm text-text-muted">
                  <input
                    type="checkbox"
                    checked={selected().has(opt.value)}
                    onChange={() => props.onToggle?.(opt.value)}
                  />
                  {opt.label}
                </label>
              )}
            </For>
          </div>
        </div>
      </Show>
    </div>
  );
}
