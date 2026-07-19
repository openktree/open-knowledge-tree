import { createSignal, For, Show } from "solid-js";
import Alert from "../../components/Alert";
import Button from "../../components/Button";
import Card from "../../components/Card";
import FormField from "../../components/FormField";
import { api } from "../../services/api";

export default function CreateReportForm(props) {
  const [title, setTitle] = createSignal("");
  const [topic, setTopic] = createSignal("");
  const [text, setText] = createSignal("");
  const [file, setFile] = createSignal(null);
  const [parentId, setParentId] = createSignal("");
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal("");

  const parentOptions = () => props.parentOptions || [];
  const canSubmit = () => !!title().trim() && (!!text().trim() || !!file()) && !submitting();

  const handleSubmit = async (e) => {
    e.preventDefault();
    if (!canSubmit()) return;
    setSubmitting(true);
    setError("");
    try {
      if (file()) {
        await api.uploadReportFile(
          props.slug,
          file(),
          title().trim(),
          topic().trim(),
          parentId().trim(),
        );
      } else {
        await api.createReport(props.slug, {
          title: title().trim(),
          topic: topic().trim(),
          text: text(),
          parent_id: parentId().trim(),
        });
      }
      setTitle("");
      setTopic("");
      setText("");
      setFile(null);
      setParentId("");
      props.onCreated?.();
    } catch (err) {
      setError(err.message);
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setSubmitting(false);
    }
  };

  const onFile = (e) => {
    const f = e.currentTarget.files?.[0];
    setFile(f || null);
    if (f && !title()) setTitle(f.name.replace(/\.(md|txt|markdown)$/i, ""));
  };

  return (
    <Card>
      <h2 class="text-lg font-semibold mb-4 dark:text-white">New Report</h2>
      <form onSubmit={handleSubmit} class="space-y-4">
        <FormField
          label="Title"
          value={title()}
          onChange={setTitle}
          placeholder="Report title"
          required
        />
        <FormField
          label="Topic (optional)"
          value={topic()}
          onChange={setTopic}
          placeholder="Topic"
        />
        <Show when={parentOptions().length > 0}>
          <div>
            <label class="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">
              Parent report (optional)
            </label>
            <select
              value={parentId()}
              onChange={(e) => setParentId(e.currentTarget.value)}
              class="w-full rounded border border-gray-300 dark:border-gray-600 dark:bg-gray-700 dark:text-white p-2 text-sm"
            >
              <option value="">— Top level —</option>
              <For each={parentOptions()}>{(r) => <option value={r.id}>{r.title}</option>}</For>
            </select>
          </div>
        </Show>
        <div>
          <label class="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">
            Body (markdown)
          </label>
          <textarea
            class="w-full rounded border border-gray-300 dark:border-gray-600 dark:bg-gray-700 dark:text-white p-2 text-sm font-mono"
            rows="6"
            value={text()}
            onInput={(e) => setText(e.currentTarget.value)}
            placeholder="# My Report&#10;&#10;Write the report in markdown..."
            disabled={!!file()}
          />
        </div>
        <div>
          <label class="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">
            Or upload a .md / .txt file
          </label>
          <input
            type="file"
            accept=".md,.markdown,.txt"
            onChange={onFile}
            class="block w-full text-sm text-gray-500 dark:text-gray-400 file:mr-3 file:py-1 file:px-3 file:rounded file:border-0 file:bg-blue-50 file:text-blue-700 hover:file:bg-blue-100 dark:file:bg-blue-900 dark:file:text-blue-100"
          />
        </div>
        <Show when={error()}>
          <Alert variant="error" message={error()} onDismiss={() => setError("")} />
        </Show>
        <div class="flex justify-end gap-2">
          <Button type="button" variant="secondary" onClick={() => props.onCancel?.()}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" loading={submitting()} disabled={!canSubmit()}>
            Create Report
          </Button>
        </div>
      </form>
    </Card>
  );
}
