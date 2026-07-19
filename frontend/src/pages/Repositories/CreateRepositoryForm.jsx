import { createResource, createSignal, For, Show } from "solid-js";
import Alert from "../../components/Alert";
import Button from "../../components/Button";
import Card from "../../components/Card";
import FormField from "../../components/FormField";
import { api } from "../../services/api";
import PresetPicker from "./PresetPicker";

/**
 * Form to create a new repository. The slug is auto-suggested from
 * the name (editable) and validated client-side.
 *
 * The database picker is sourced from
 * `GET /admin/databases`, which returns the configured
 * allow-list plus the default. The server is the source of truth:
 * if the caller isn't permitted to pick a non-default database,
 * the server silently overrides to the default on POST. The
 * picker in this form is therefore advisory — we render whatever
 * the endpoint returns, and we surface the backend's
 * `default_warning` string verbatim whenever the user has the
 * default selected, so the trade-off is visible at the moment of
 * decision.
 *
 * Props:
 *   - onCreated: () => void  — called after a successful POST so the
 *                              parent can refetch and clear the form.
 *   - onAlert:    (alert) => void — { variant, message } | null
 */
export default function CreateRepositoryForm(props) {
  const [name, setName] = createSignal("");
  const [slug, setSlug] = createSignal("");
  const [description, setDescription] = createSignal("");
  const [databaseName, setDatabaseName] = createSignal("");
  const [seed, setSeed] = createSignal({ preset: "", providers: {}, contexts: [] });
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal("");
  const [slugTouched, setSlugTouched] = createSignal(false);

  // The picker source. We let the resource suspend naturally; if
  // the endpoint is unreachable (e.g. the user isn't a sys admin
  // and the route is denied), we fall back to an empty list and
  // the form still works because the server picks the default
  // when the field is empty.
  const [dbs] = createResource(() => api.listRepositoryDatabases().catch(() => null));
  const dbList = () => dbs()?.databases || [];
  const defaultDB = () => dbs()?.default || "default";
  const defaultWarning = () => dbs()?.default_warning || "";

  // Initialize the picker to the configured default once the
  // resource resolves. Doing this in a `createEffect` would
  // overwrite the user's choice on every render; this explicit
  // one-shot is intentional.
  let initialized = false;
  const ensureInitialized = () => {
    if (initialized) return;
    if (!dbs()) return;
    initialized = true;
    setDatabaseName(defaultDB());
  };

  const isDefaultSelected = () => {
    ensureInitialized();
    const list = dbList();
    const current = databaseName();
    if (!current) return true;
    return list.some((d) => d.name === current && d.is_default);
  };

  const slugify = (input) =>
    (input || "")
      .toLowerCase()
      .trim()
      .replace(/[^a-z0-9]+/g, "-")
      .replace(/^-+|-+$/g, "")
      .slice(0, 64);

  const onNameChange = (value) => {
    setName(value);
    if (!slugTouched()) {
      setSlug(slugify(value));
    }
  };

  const onSlugChange = (value) => {
    setSlugTouched(true);
    setSlug(slugify(value));
  };

  const slugError = () => {
    const s = slug();
    if (!s) return "Slug is required";
    if (!/^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/.test(s)) {
      return "Slug must be lowercase alphanumeric with optional dashes";
    }
    return "";
  };

  const canSubmit = () => !!name().trim() && !slugError() && !submitting();
  const hasOverrides = () => {
    const s = seed();
    return (s.providers && (s.providers.search?.length || s.providers.resolution?.length)) || false;
  };

  const handleSubmit = async (e) => {
    e.preventDefault();
    if (!canSubmit()) return;
    setSubmitting(true);
    setError("");
    try {
      await api.createRepository({
        name: name().trim(),
        slug: slug(),
        description: description().trim(),
        database_name: databaseName() || "",
        preset: seed().preset || "",
        providers: hasOverrides() ? seed().providers : undefined,
        contexts: seed().contexts && seed().contexts.length ? seed().contexts : undefined,
      });
      setName("");
      setSlug("");
      setDescription("");
      setDatabaseName(defaultDB());
      setSlugTouched(false);
      props.onCreated?.();
    } catch (err) {
      setError(err.message);
      props.onAlert?.({ variant: "error", message: err.message });
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Card>
      <h2 class="text-lg font-semibold mb-4 dark:text-white">Create Repository</h2>
      <form onSubmit={handleSubmit} class="space-y-4">
        <FormField
          label="Name"
          value={name()}
          onChange={onNameChange}
          placeholder="My Knowledge Tree"
          required
        />
        <FormField
          label="Slug"
          value={slug()}
          onChange={onSlugChange}
          placeholder="my-knowledge-tree"
          required
          error={slug() && slugError()}
        />
        <FormField
          label="Description"
          type="text"
          value={description()}
          onChange={setDescription}
          placeholder="Optional short description"
        />

        <PresetPicker value={seed} onChange={setSeed} />

        <Show when={dbList().length > 0}>
          <FormField
            label="Database"
            type="select"
            name="database_name"
            value={databaseName()}
            onChange={setDatabaseName}
          >
            <For each={dbList()}>
              {(db) => (
                <option value={db.name}>
                  {db.name}
                  {db.is_default ? " (default)" : ""}
                </option>
              )}
            </For>
          </FormField>
        </Show>

        <Show when={isDefaultSelected() && defaultWarning()}>
          <Alert variant="warning" message={defaultWarning()} />
        </Show>

        <Alert variant="error" message={error()} onDismiss={() => setError("")} />

        <div class="flex justify-end">
          <Button type="submit" disabled={!canSubmit()} loading={submitting()}>
            Create
          </Button>
        </div>
      </form>
    </Card>
  );
}
