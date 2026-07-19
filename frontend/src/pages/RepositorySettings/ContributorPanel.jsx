import { createSignal, Show } from "solid-js";
import { api } from "../../services/api";
import Card from "../../components/Card";

// ContributorPanel is the per-repo contributor identity section of
// the RepositorySettings page. The repo's decompositions pushed to
// the registry (contribute_source) carry a contributor object so
// pulling repos can see who contributed a source. By default every
// repo contributes anonymously; an admin can opt out and set a
// display name here.
//
// The panel reads its initial state from the settings resource
// (settings.contributor_display_name + settings.contributor_anonymous,
// surfaced by GetSettings) so the page load shows the current value
// without a second round-trip. Save PUTs to /settings/contributor
// and alerts the parent via onAlert.
//
// Props:
//   repoID      – accessor string  repository UUID
//   displayName – accessor string|null  initial display name from GetSettings
//   anonymous   – accessor bool    initial anonymous flag from GetSettings
//   onChanged   – () => void       notify parent to refetch settings
//   onAlert     – (alert) => void  surface save errors / success
export default function ContributorPanel(props) {
  const [busy, setBusy] = createSignal(false);
  // Local editable state. Seeded from the server-provided props the
  // first time the settings resource resolves a non-undefined value.
  const [anonymous, setAnonymous] = createSignal(true);
  const [displayName, setDisplayName] = createSignal("");
  const [seeded, setSeeded] = createSignal(false);

  // Seed once when the settings resource resolves. We don't use
  // createEffect here because the parent's settings resource may
  // re-resolve on save and we don't want to clobber the admin's
  // in-progress edits.
  const seedFromServer = () => {
    if (seeded()) return;
    const a = props.anonymous?.();
    const n = props.displayName?.();
    if (a === undefined) return;
    setAnonymous(a);
    setDisplayName(n ?? "");
    setSeeded(true);
  };
  seedFromServer();

  const handleSave = async () => {
    setBusy(true);
    try {
      const body = anonymous()
        ? { anonymous: true, display_name: null }
        : { anonymous: false, display_name: displayName().trim() };
      await api.setRepositoryContributor(props.repoID(), body);
      props.onAlert?.({ variant: "success", message: "Contributor identity saved." });
      props.onChanged?.();
    } catch (err) {
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setBusy(false);
    }
  };

  const dirty = () => {
    if (!seeded()) return false;
    if (anonymous() !== props.anonymous?.()) return true;
    const serverName = props.displayName?.() ?? "";
    if (!anonymous() && displayName().trim() !== serverName) return true;
    return false;
  };

  const canSave = () => {
    if (!dirty() || busy()) return false;
    if (!anonymous() && displayName().trim() === "") return false;
    return true;
  };

  return (
    <Card>
      <h3 class="text-lg font-semibold mb-1 dark:text-white">Contributor Identity</h3>
      <p class="text-sm text-gray-500 dark:text-gray-400 mb-4">
        Choose how this repository is attributed when it contributes sources to the
        registry. When anonymous, the registry records your pushes without a name. Turn
        anonymity off and set a display name so other repositories can see who
        contributed each decomposition.
      </p>
      <div class="space-y-3">
        <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
          <input
            type="checkbox"
            checked={anonymous()}
            onInput={(e) => setAnonymous(e.currentTarget.checked)}
            disabled={busy()}
            class="rounded border-gray-300 dark:border-gray-600 dark:bg-gray-900"
          />
          <span>Contribute anonymously</span>
        </label>
        <Show when={!anonymous()}>
          <div>
            <label class="block text-sm font-medium text-gray-700 dark:text-gray-300 mb-1">
              Display name
            </label>
            <input
              type="text"
              value={displayName()}
              onInput={(e) => setDisplayName(e.currentTarget.value)}
              disabled={busy()}
              placeholder="e.g. Alice's Research Lab"
              maxlength={120}
              class="w-full text-sm px-3 py-2 rounded border border-gray-300 dark:border-gray-600 bg-white dark:bg-gray-700 text-gray-700 dark:text-gray-200 disabled:opacity-50"
            />
            <p class="text-xs text-gray-400 dark:text-gray-500 mt-1">
              Shown to pulling repos on every source this repo contributes. Up to 120 characters.
            </p>
          </div>
        </Show>
        <Show when={anonymous()}>
          <p class="text-xs text-gray-400 dark:text-gray-500">
            The registry will record your pushes with the canonical <em>anonymous</em> marker;
            no display name is sent.
          </p>
        </Show>
        <div class="flex items-center gap-3">
          <button
            type="button"
            disabled={!canSave()}
            onClick={handleSave}
            class="text-sm px-3 py-1.5 rounded border bg-gray-100 text-gray-700 dark:bg-gray-700 dark:text-gray-200 border-gray-300 dark:border-gray-600 hover:bg-gray-200 dark:hover:bg-gray-600 disabled:opacity-50"
          >
            {busy() ? "Saving…" : "Save Contributor Identity"}
          </button>
        </div>
      </div>
    </Card>
  );
}