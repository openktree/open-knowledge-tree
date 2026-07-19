import { createResource, createSignal, For, Show } from "solid-js";
import Badge from "../../components/Badge";
import Button from "../../components/Button";
import CollapsibleSection from "../../components/CollapsibleSection";
import SearchInput from "../../components/SearchInput";
import { api } from "../../services/api";
import { formatTimestamp, statusVariant } from "../Sources/constants";

export default function AddExistingSourcePicker(props) {
  const [q, setQ] = createSignal("");
  const [offset, setOffset] = createSignal(0);
  const [open, setOpen] = createSignal(false);
  const [busyID, setBusyID] = createSignal("");
  const [error, setError] = createSignal("");
  const [refreshKey, setRefreshKey] = createSignal(0);

  const [srcData] = createResource(
    () => [open(), q(), offset(), refreshKey()],
    async ([isOpen, query, off]) => {
      if (!isOpen) return { data: [], total: 0, limit: 100, offset: 0 };
      try {
        return await api.listSources(props.slug, { q: query, offset: off, limit: 50 });
      } catch {
        return { data: [], total: 0, limit: 100, offset: 0 };
      }
    },
  );

  const sources = () => srcData()?.data || [];
  const total = () => srcData()?.total || 0;

  const onAdd = async (src) => {
    setBusyID(src.id);
    setError("");
    try {
      await api.addInvestigationSource(props.slug, props.invID, src.id);
      props.onAdded?.();
    } catch (err) {
      setError(err.message);
    } finally {
      setBusyID("");
    }
  };

  return (
    <CollapsibleSection
      title="Add an existing source"
      subtitle="Add a source already in this repository to the investigation."
      defaultOpen={false}
      onToggle={setOpen}
    >
      <div class="space-y-3">
        <SearchInput
          placeholder="Search repo sources..."
          onSearch={(v) => {
            setQ(v);
            setOffset(0);
          }}
        />
        <Show when={error()}>
          <p class="text-xs text-red-500">{error()}</p>
        </Show>
        <Show when={sources().length === 0 && !srcData.loading}>
          <p class="text-sm text-gray-400 dark:text-gray-500">
            No sources found{q() ? ` for "${q()}"` : ""}.
          </p>
        </Show>
        <div class="space-y-2 max-h-96 overflow-y-auto">
          <For each={sources()}>
            {(src) => (
              <div class="border rounded dark:border-gray-700 p-2 flex items-center justify-between gap-2">
                <div class="min-w-0 flex-1">
                  <p class="text-sm truncate dark:text-gray-200">
                    {src.parsed_title && src.parsed_title.trim().length > 0
                      ? src.parsed_title
                      : src.url}
                  </p>
                  <div class="flex items-center gap-2 mt-0.5 flex-wrap text-xs text-gray-500 dark:text-gray-400">
                    <Badge variant={statusVariant(src.status)}>{src.status}</Badge>
                    <Show when={src.fetched_at && src.fetched_at.Valid}>
                      <span>{formatTimestamp(src.fetched_at)}</span>
                    </Show>
                  </div>
                </div>
                <Button
                  variant="secondary"
                  class="text-xs px-2 py-1"
                  disabled={busyID() === src.id}
                  loading={busyID() === src.id}
                  onClick={() => onAdd(src)}
                >
                  Add
                </Button>
              </div>
            )}
          </For>
        </div>
        <Show when={total() > 50}>
          <p class="text-xs text-gray-500 dark:text-gray-400">
            Showing first 50 of {total()}. Refine the search to narrow.
          </p>
        </Show>
      </div>
    </CollapsibleSection>
  );
}
