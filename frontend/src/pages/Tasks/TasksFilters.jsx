import { For } from "solid-js";
import Button from "../../components/Button";
import Card from "../../components/Card";
import FormField from "../../components/FormField";
import { KIND_OPTIONS, QUEUE_OPTIONS, STATE_OPTIONS } from "./constants";

// TasksFilters renders the state/kind/queue dropdowns and the
// Refresh button. The page owns the filter signals; this
// component is controlled via props and calls back via the
// onXxx handlers.
export default function TasksFilters(props) {
  return (
    <Card>
      <h2 class="text-lg font-semibold mb-4 dark:text-white">Task Filters</h2>
      <div class="flex flex-wrap gap-4">
        <FormField
          type="select"
          label="State"
          value={props.state}
          onChange={props.onStateChange}
          class="min-w-[160px]"
        >
          <For each={STATE_OPTIONS}>{(opt) => <option value={opt.value}>{opt.label}</option>}</For>
        </FormField>
        <FormField
          type="select"
          label="Kind"
          value={props.kind}
          onChange={props.onKindChange}
          class="min-w-[160px]"
        >
          <For each={KIND_OPTIONS}>{(opt) => <option value={opt.value}>{opt.label}</option>}</For>
        </FormField>
        <FormField
          type="select"
          label="Queue"
          value={props.queue}
          onChange={props.onQueueChange}
          class="min-w-[160px]"
        >
          <For each={QUEUE_OPTIONS}>{(opt) => <option value={opt.value}>{opt.label}</option>}</For>
        </FormField>
        <div class="flex items-end">
          <Button
            variant="secondary"
            onClick={props.onRefresh}
            loading={props.loading}
            loadingText="..."
          >
            Refresh
          </Button>
        </div>
      </div>
    </Card>
  );
}
