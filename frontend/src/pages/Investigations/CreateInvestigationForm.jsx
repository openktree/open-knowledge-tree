import { createSignal, Show } from "solid-js";
import { api } from "../../services/api";
import Button from "../../components/Button";
import Card from "../../components/Card";
import FormField from "../../components/FormField";
import Alert from "../../components/Alert";
import InfoBanner from "./InfoBanner";

export default function CreateInvestigationForm(props) {
  const [title, setTitle] = createSignal("");
  const [topic, setTopic] = createSignal("");
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal("");
  const [showInfo, setShowInfo] = createSignal(false);

  const canSubmit = () => !!title().trim() && !submitting();

  const handleSubmit = async (e) => {
    e.preventDefault();
    if (!canSubmit()) return;
    setSubmitting(true);
    setError("");
    try {
      await api.createInvestigation(props.slug, {
        title: title().trim(),
        topic: topic().trim(),
      });
      setTitle("");
      setTopic("");
      props.onCreated?.();
    } catch (err) {
      setError(err.message);
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Card class="relative">
      <div class="flex items-center gap-2 mb-4">
        <h2 class="text-lg font-semibold dark:text-white">Create Investigation</h2>
        <div
          onMouseEnter={() => setShowInfo(true)}
          onMouseLeave={() => setShowInfo(false)}
        >
          <svg
            xmlns="http://www.w3.org/2000/svg"
            class="h-4 w-4 text-gray-400 dark:text-gray-500 cursor-help"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            stroke-width="2"
            stroke-linecap="round"
            stroke-linejoin="round"
          >
            <path d="M13 16h-1v-4h-1m1-4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
          </svg>
        </div>
      </div>
      <Show when={showInfo()}>
        <div class="absolute left-4 right-4 z-20" style={{ top: "3.5rem" }}>
          <InfoBanner />
        </div>
      </Show>
      <form onSubmit={handleSubmit} class="space-y-4">
        <FormField
          label="Title"
          value={title()}
          onChange={setTitle}
          placeholder="Climate impacts on coastal cities"
          required
        />
        <FormField
          label="Topic"
          value={topic()}
          onChange={setTopic}
          placeholder="Optional free-text research topic"
        />
        <Alert variant="error" message={error()} onDismiss={() => setError("")} />
        <div class="flex justify-end gap-2">
          <Button type="button" variant="secondary" onClick={() => props.onCancel?.()}>
            Cancel
          </Button>
          <Button type="submit" disabled={!canSubmit()} loading={submitting()}>
            Create
          </Button>
        </div>
      </form>
    </Card>
  );
}