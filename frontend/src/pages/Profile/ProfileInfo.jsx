import { createSignal, Show } from "solid-js";
import Alert from "../../components/Alert";
import Button from "../../components/Button";
import Card from "../../components/Card";
import FormField from "../../components/FormField";

/**
 * ProfileInfo — the "Profile" tab. Shows the user's email (read-only)
 * and an editable display_name. Saving calls onUpdateProfile with
 * {display_name}; the parent issues the API call and surfaces the
 * result via onAlert.
 *
 * Props:
 *   - user:           accessor () => {id, email, display_name} | null
 *   - onUpdateProfile: (body) => Promise  — parent runs api.updateProfile
 *   - onAlert:        (alert) => void     — { variant, message } | null
 */
export default function ProfileInfo(props) {
  const [editing, setEditing] = createSignal(false);
  const [displayName, setDisplayName] = createSignal("");
  const [saving, setSaving] = createSignal(false);
  const [localError, setLocalError] = createSignal("");

  const startEdit = () => {
    setDisplayName(props.user()?.display_name || "");
    setLocalError("");
    setEditing(true);
  };

  const cancel = () => {
    setEditing(false);
    setLocalError("");
  };

  const save = async (e) => {
    e.preventDefault();
    const user = props.user();
    if (!user) return;
    setSaving(true);
    try {
      await props.onUpdateProfile({ display_name: displayName() });
      props.onAlert?.({ variant: "success", message: "Profile updated" });
      setEditing(false);
    } catch (err) {
      setLocalError(err.message);
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setSaving(false);
    }
  };

  return (
    <Card>
      <div class="flex items-center justify-between mb-4">
        <h2 class="text-lg font-semibold dark:text-white">Profile</h2>
        <Show when={!editing()}>
          <Button variant="secondary" onClick={startEdit}>
            Edit
          </Button>
        </Show>
      </div>

      <div class="space-y-4">
        <div>
          <div class="text-xs font-medium uppercase tracking-wide text-text-muted mb-1">Email</div>
          <div class="text-sm text-text-base">{props.user()?.email || "\u2014"}</div>
        </div>

        <Show
          when={!editing()}
          fallback={
            <form onSubmit={save} class="space-y-3">
              <FormField
                label="Display name"
                value={displayName()}
                onChange={setDisplayName}
                placeholder="Your name"
              />
              <div class="flex gap-2">
                <Button
                  type="submit"
                  disabled={saving()}
                  loading={saving()}
                  loadingText="Saving..."
                >
                  Save
                </Button>
                <Button variant="secondary" onClick={cancel} disabled={saving()}>
                  Cancel
                </Button>
              </div>
              <Alert variant="error" message={localError()} onDismiss={() => setLocalError("")} />
            </form>
          }
        >
          <div>
            <div class="text-xs font-medium uppercase tracking-wide text-text-muted mb-1">
              Display name
            </div>
            <div class="text-sm text-text-base">{props.user()?.display_name || "\u2014"}</div>
          </div>
        </Show>

        <div>
          <div class="text-xs font-medium uppercase tracking-wide text-text-muted mb-1">
            User ID
          </div>
          <div class="text-xs text-text-muted font-mono break-all">
            {props.user()?.id || "\u2014"}
          </div>
        </div>
      </div>
    </Card>
  );
}
