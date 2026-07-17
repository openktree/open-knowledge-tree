import { Show, For, createSignal } from "solid-js";

// TraceTable renders the per-chunk / per-image troubleshooting traces
// recorded by the source_decomposition worker (chunk_traces and
// image_traces in the job's recorded output). It is private to the
// Tasks page folder because it's specific to the source_decomposition
// output shape. Each trace block is collapsible; when collapsed only
// the summary line (count + failures) is shown, when expanded the raw
// JSON array is displayed in a <pre> for easy scanning/copying.
//
// The component is a controlled presentational piece: it takes the job
// output object and derives the two arrays from it; it owns only the
// local collapse state.
export default function TraceTable(props) {
  const chunks = () => (props.output && props.output.chunk_traces) || [];
  const images = () => (props.output && props.output.image_traces) || [];
  const chunkFailures = () => chunks().filter((c) => c.error).length;
  const imageFailures = () =>
    images().filter((i) => i.error || i.skipped).length;

  return (
    <Show when={chunks().length > 0 || images().length > 0}>
      <div class="mt-4 space-y-3">
        <Show when={chunks().length > 0}>
          <TraceBlock
            label="Chunk traces"
            count={chunks().length}
            failures={chunkFailures()}
            data={chunks()}
          />
        </Show>
        <Show when={images().length > 0}>
          <TraceBlock
            label="Image traces"
            count={images().length}
            failures={imageFailures()}
            data={images()}
          />
        </Show>
      </div>
    </Show>
  );
}

function TraceBlock(props) {
  const [open, setOpen] = createSignal(false);
  return (
    <div class="border border-gray-200 dark:border-gray-700 rounded">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        class="w-full flex items-center justify-between px-3 py-2 text-sm text-left bg-gray-50 dark:bg-gray-900 hover:bg-gray-100 dark:hover:bg-gray-800"
      >
        <span class="font-medium dark:text-gray-200">
          {props.label}: {props.count}{" "}
          <Show when={props.failures > 0}>
            <span class="text-red-600 dark:text-red-400">
              ({props.failures} failed)
            </span>
          </Show>
        </span>
        <span class="text-xs text-gray-500 dark:text-gray-400">
          {open() ? "Collapse" : "Expand"}
        </span>
      </button>
      <Show when={open()}>
        <pre class="text-xs bg-gray-50 dark:bg-gray-900 p-3 overflow-x-auto font-mono text-gray-700 dark:text-gray-300">
          {JSON.stringify(props.data, null, 2)}
        </pre>
      </Show>
    </div>
  );
}