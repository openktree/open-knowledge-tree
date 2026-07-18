import { createResource, createSignal, Show } from "solid-js";
import { api } from "../../services/api";
import Layout from "../../components/Layout";
import Alert from "../../components/Alert";
import Card from "../../components/Card";
import Loading from "../../components/Loading";
import PromptsetsTable from "./PromptsetsTable";
import PromptsetForm from "./PromptsetForm";
import { emptyDraft, draftFromPromptset } from "./constants";

// Promptsets is the route entry for /promptsets. It owns the
// page-level state (list, draft, alert, busy) and delegates
// rendering to PromptsetsTable + PromptsetForm. Stays under the
// 100-line budget per the Page folder convention.
export default function Promptsets() {
  const [promptsets, { refetch }] = createResource(() => api.listPromptsets());
  const [alert, setAlert] = createSignal(null);
  const [draft, setDraft] = createSignal(null);
  const [editing, setEditing] = createSignal(null);
  const [busyHash, setBusyHash] = createSignal(null);

  const startCreate = () => { setDraft(emptyDraft()); setEditing("create"); setAlert(null); };
  const startEdit = (ps) => { setDraft(draftFromPromptset(ps)); setEditing({ hash: ps.hash }); setAlert(null); };
  const cancel = () => { setDraft(null); setEditing(null); };
  const onChange = (field, value) => setDraft((d) => ({ ...d, [field]: value }));

  const submit = async () => {
    const d = draft();
    if (!d.name?.trim()) { setAlert({ variant: "error", message: "Name is required." }); return; }
    setBusyHash("form");
    try {
      if (editing() === "create") {
        await api.createPromptset(d);
        setAlert({ variant: "success", message: "Promptset created." });
      } else {
        await api.updatePromptset(editing().hash, d);
        setAlert({ variant: "success", message: "Saved as a new philosophy (hash changed)." });
      }
      cancel();
      refetch();
    } catch (err) {
      setAlert({ variant: "error", message: err.message });
    } finally {
      setBusyHash(null);
    }
  };

  const remove = async (ps) => {
    if (!confirm(`Delete promptset "${ps.name}"? Repositories pointing at it fall back to the global default.`)) return;
    setBusyHash(ps.hash);
    try {
      await api.deletePromptset(ps.hash);
      setAlert({ variant: "success", message: "Promptset deleted." });
      refetch();
    } catch (err) {
      setAlert({ variant: "error", message: err.message });
    } finally {
      setBusyHash(null);
    }
  };

  return (
    <Layout>
      <div class="space-y-6">
        <Alert variant={alert()?.variant} message={alert()?.message} onDismiss={() => setAlert(null)} />
        <div class="flex items-center justify-between">
          <div>
            <h1 class="text-2xl font-bold text-gray-900 dark:text-white">Promptsets</h1>
            <p class="text-sm text-gray-500 dark:text-gray-400 mt-1">
              A promptset is the complete set of phase prompts a repository decomposes under. The hash is the identity.
            </p>
          </div>
          <Show when={!editing()}>
            <button type="button" onClick={startCreate} class="px-3 py-1.5 text-sm rounded bg-blue-600 text-white hover:bg-blue-700">New Promptset</button>
          </Show>
        </div>
        <Show when={editing()}>
          <Card>
            <h3 class="text-lg font-semibold mb-3 dark:text-white">{editing() === "create" ? "Create Promptset" : "Edit Promptset"}</h3>
            <PromptsetForm draft={draft} onChange={onChange} onSubmit={submit} onCancel={cancel} busy={() => busyHash() === "form"} submitLabel={editing() === "create" ? "Create" : "Save"} />
          </Card>
        </Show>
        <Show when={!promptsets.loading} fallback={<Loading message="Loading promptsets…" />}>
          <Card><PromptsetsTable promptsets={promptsets} onEdit={startEdit} onDelete={remove} busyHash={busyHash} /></Card>
        </Show>
      </div>
    </Layout>
  );
}