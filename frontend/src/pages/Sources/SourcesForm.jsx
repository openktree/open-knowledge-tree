import { Show } from "solid-js";
import Button from "../../components/Button";
import Card from "../../components/Card";
import FormField from "../../components/FormField";

export default function SourcesForm(props) {
  return (
    <Show when={props.canCreate}>
      <Card class="mb-6">
        <h2 class="text-lg font-semibold mb-1 dark:text-white">Add a source</h2>
        <p class="text-sm text-gray-500 dark:text-gray-400 mb-4">
          Track a URL under this repository. Use the Providers page to search for and retrieve
          source content.
        </p>
        <form onSubmit={props.onAdd} class="flex gap-2">
          <FormField
            value={props.addURL()}
            onChange={props.onChangeURL}
            placeholder="https://example.com/paper"
            class="flex-1"
          />
          <FormField type="select" value={props.addKind()} onChange={props.onChangeKind}>
            <option value="homepage">homepage</option>
            <option value="paper">paper</option>
            <option value="dataset">dataset</option>
            <option value="code">code</option>
            <option value="other">other</option>
          </FormField>
          <Button type="submit" loading={props.creating()} loadingText="Adding...">
            Add
          </Button>
          <Button type="button" variant="secondary" onClick={props.onCancel}>
            Cancel
          </Button>
        </form>
      </Card>
    </Show>
  );
}
