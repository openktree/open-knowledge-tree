import { createSignal, For, Show } from "solid-js";
import Alert from "../../components/Alert";
import Button from "../../components/Button";
import FormField from "../../components/FormField";
import Modal from "../../components/Modal";
import { ALL_REPOS, EXPIRY_OPTIONS, PERMISSION_CATEGORIES } from "./constants";
import PermissionCategorySection from "./PermissionCategorySection";

/**
 * CreateTokenModal — the "Create API token" dialog. Collects name,
 * permissions (multi-select via checkboxes), repository scope, and
 * expiry; calls onCreate with the payload and shows the raw token
 * exactly once on success (with a copy button + a "you won't see
 * this again" warning).
 *
 * Props:
 *   - open:        boolean accessor
 *   - onClose:     () => void
 *   - repositories: accessor () => Array<{id, name}> | null
 *   - onCreate:    (payload) => Promise<{token, prefix, ...}>
 *   - onAlert:     (alert) => void
 */
export default function CreateTokenModal(props) {
  const [name, setName] = createSignal("");
  const [selectedPerms, setSelectedPerms] = createSignal(new Set());
  const [repo, setRepo] = createSignal(ALL_REPOS);
  const [expiry, setExpiry] = createSignal(0);
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal("");
  const [createdToken, setCreatedToken] = createSignal("");
  const [copied, setCopied] = createSignal(false);

  const reset = () => {
    setName("");
    setSelectedPerms(new Set());
    setRepo(ALL_REPOS);
    setExpiry(0);
    setError("");
    setCreatedToken("");
    setCopied(false);
  };

  const close = () => {
    reset();
    props.onClose?.();
  };

  const togglePerm = (value) => {
    let next = new Set(selectedPerms());
    if (value === "*:*") {
      // "All permissions" is exclusive — clears the others.
      if (next.has(value)) next.clear();
      else next = new Set(["*:*"]);
    } else {
      next.delete("*:*");
      if (next.has(value)) next.delete(value);
      else next.add(value);
    }
    setSelectedPerms(next);
  };

  const submit = async (e) => {
    e.preventDefault();
    if (!name().trim()) {
      setError("Name is required");
      return;
    }
    const perms = [...selectedPerms()];
    if (perms.length === 0) {
      setError("Select at least one permission");
      return;
    }
    setSubmitting(true);
    setError("");
    const payload = {
      name: name().trim(),
      permissions: perms,
    };
    if (repo() !== ALL_REPOS) payload.repository_id = repo();
    if (expiry() > 0) payload.expires_in_days = expiry();

    try {
      const result = await props.onCreate(payload);
      setCreatedToken(result.token || "");
    } catch (err) {
      setError(err.message);
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setSubmitting(false);
    }
  };

  const copyToken = async () => {
    try {
      await navigator.clipboard.writeText(createdToken());
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } catch {
      setError("Copy failed — select and copy the token manually");
    }
  };

  return (
    <Modal
      open={props.open}
      onClose={close}
      title={createdToken() ? "Token created" : "Create API token"}
    >
      <Show
        when={!createdToken()}
        fallback={
          <div class="space-y-4">
            <Alert
              variant="warning"
              message="Copy this token now — you won't be able to see it again."
            />
            <div class="bg-gray-50 dark:bg-gray-800 border border-border rounded p-3 font-mono text-xs break-all max-h-32 overflow-y-auto">
              {createdToken()}
            </div>
            <div class="flex gap-2">
              <Button onClick={copyToken} variant="secondary">
                {copied() ? "Copied!" : "Copy token"}
              </Button>
              <Button onClick={close} variant="primary">
                Done
              </Button>
            </div>
          </div>
        }
      >
        <form onSubmit={submit} class="space-y-4">
          <FormField
            label="Name"
            value={name()}
            onChange={setName}
            placeholder="e.g. CI ingest bot"
            required
          />

          <div>
            <div class="block mb-1 text-sm font-medium text-text-base">Permissions</div>
            <div class="space-y-1 max-h-60 overflow-y-auto">
              <For each={PERMISSION_CATEGORIES}>
                {(cat, i) => (
                  <PermissionCategorySection
                    category={cat}
                    selected={selectedPerms}
                    onToggle={togglePerm}
                    // The first category ("All permissions") defaults
                    // open so the admin shortcut is immediately visible
                    // without an extra click; the rest start collapsed.
                    defaultOpen={i() === 0}
                  />
                )}
              </For>
            </div>
          </div>

          <FormField label="Repository scope" type="select" value={repo()} onChange={setRepo}>
            <option value={ALL_REPOS}>All repositories I can access</option>
            <For each={props.repositories?.() || []}>
              {(r) => <option value={r.id}>{r.name}</option>}
            </For>
          </FormField>

          <FormField
            label="Expiry"
            type="select"
            value={String(expiry())}
            onChange={(v) => setExpiry(Number(v))}
          >
            <For each={EXPIRY_OPTIONS}>
              {(opt) => <option value={String(opt.value)}>{opt.label}</option>}
            </For>
          </FormField>

          <Alert variant="error" message={error()} onDismiss={() => setError("")} />

          <div class="flex gap-2 justify-end">
            <Button type="button" variant="secondary" onClick={close} disabled={submitting()}>
              Cancel
            </Button>
            <Button
              type="submit"
              disabled={submitting()}
              loading={submitting()}
              loadingText="Creating..."
            >
              Create token
            </Button>
          </div>
        </form>
      </Show>
    </Modal>
  );
}
