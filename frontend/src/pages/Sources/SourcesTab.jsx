import { createSignal, createResource, For, Show } from "solid-js";
import { A } from "@solidjs/router";
import { api } from "../../services/api";
import { useRepository } from "../../store/repository";
import Alert from "../../components/Alert";
import Badge from "../../components/Badge";
import Button from "../../components/Button";
import Card from "../../components/Card";
import EmptyState from "../../components/EmptyState";
import FormField from "../../components/FormField";
import { statusVariant, formatTimestamp } from "./constants";

/**
 * Body of the "Sources" tab. Lists every row in the active
 * repository's `sources` table. Clicking a row opens the
 * dedicated SourceDetail page at
 * /:slug/sources/:sourceID, which is the
 * shareable URL operators can send to a teammate.
 *
 * The list never renders the raw fetched body. The detail
 * page is the only place that displays the extracted
 * content (parsed title, text, images).
 *
 * State ownership:
 *   - The list itself is a `createResource` keyed on the
 *     current repository id, so changing repositories in the
 *     Layout dropdown refreshes the list automatically.
 *   - The "add source" form and per-row delete state are
 *     owned here. The detail page owns its own load
 *     lifecycle, so the list only needs to refetch on
 *     add/delete.
 */
export default function SourcesTab(props) {
  const repo = useRepository();

  const [alert, setAlert] = createSignal(null);
  const [addURL, setAddURL] = createSignal("");
  const [addKind, setAddKind] = createSignal("homepage");
  const [creating, setCreating] = createSignal(false);
  const [deletingID, setDeletingID] = createSignal("");

  const [sources, { refetch }] = createResource(
    () => (repo.currentRepo() ? repo.currentRepo().slug : ""),
    async (slug) => {
      if (!slug) return null;
      try {
        const data = await api.listSources(slug);
        return data.sources || [];
      } catch (err) {
        setAlert({ variant: "error", message: err.message });
        return [];
      }
    }
  );

  const handleDelete = async (source) => {
    const slug = repo.currentRepo()?.slug;
    if (!slug) return;
    if (!window.confirm(`Delete source "${source.url}"? This cannot be undone.`)) return;
    setDeletingID(source.id);
    setAlert(null);
    try {
      await api.deleteSource(slug, source.id);
      refetch();
    } catch (err) {
      setAlert({ variant: "error", message: err.message });
    } finally {
      setDeletingID("");
    }
  };

  const handleAdd = async (e) => {
    e.preventDefault();
    if (!addURL().trim()) return;
    const slug = repo.currentRepo()?.slug;
    if (!slug) return;
    setCreating(true);
    setAlert(null);
    try {
      await fetch(`/api/v1/repositories/${slug}/sources`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${localStorage.getItem("token")}`,
          "X-Repository-ID": repo.currentRepo().id,
        },
        body: JSON.stringify({ url: addURL().trim(), kind: addKind() }),
      }).then(async (r) => {
        if (!r.ok) {
          const data = await r.json().catch(() => ({}));
          throw new Error(data.error || "failed to create source");
        }
      });
      setAddURL("");
      setAddKind("homepage");
      refetch();
    } catch (err) {
      setAlert({ variant: "error", message: err.message });
    } finally {
      setCreating(false);
    }
  };

  return (
    <div>
      <Alert
        variant={alert()?.variant}
        message={alert()?.message}
        onDismiss={() => setAlert(null)}
      />

      <Show when={props.canCreate?.() !== false}>
        <Card class="mb-6">
          <h2 class="text-lg font-semibold mb-1 dark:text-white">Add a source</h2>
          <p class="text-sm text-gray-500 dark:text-gray-400 mb-4">
            Track a URL under this repository. The fetch source task on the
            Providers tab can be used to actually pull and parse the content.
          </p>
          <form onSubmit={handleAdd} class="flex gap-2">
            <FormField
              value={addURL()}
              onChange={setAddURL}
              placeholder="https://example.com/paper"
              class="flex-1"
            />
            <FormField
              type="select"
              value={addKind()}
              onChange={setAddKind}
            >
              <option value="homepage">homepage</option>
              <option value="paper">paper</option>
              <option value="dataset">dataset</option>
              <option value="code">code</option>
              <option value="other">other</option>
            </FormField>
            <Button
              type="submit"
              loading={creating()}
              loadingText="Adding..."
            >
              Add
            </Button>
          </form>
        </Card>
      </Show>

      <Card>
        <div class="flex items-center justify-between mb-3">
          <h2 class="text-lg font-semibold dark:text-white">Fetched sources</h2>
          <Button
            variant="secondary"
            onClick={refetch}
            class="text-xs px-2 py-1"
            loading={sources.loading}
            loadingText="Refreshing..."
          >
            Refresh
          </Button>
        </div>
        <p class="text-sm text-gray-500 dark:text-gray-400 mb-4">
          Sources created in this repository. Open a row to read the extracted
          content, view images, and copy a shareable link.
        </p>

        <Show
          when={repo.currentRepo()}
          fallback={
            <EmptyState
              title="Select a repository to view its sources."
              description="Use the repository dropdown in the top bar."
            />
          }
        >
          <Show
            when={!sources.loading}
            fallback={
              <p class="text-sm text-gray-400 dark:text-gray-500">Loading sources...</p>
            }
          >
            <Show
              when={sources() && sources().length > 0}
              fallback={
                <EmptyState
                  title="No sources yet."
                  description="Add one above or enqueue a Retrieve Source job on the Providers tab."
                />
              }
            >
              <div class="space-y-2">
                <For each={sources()}>
                  {(source) => (
                    <SourceRow
                      source={source}
                      slug={repo.currentRepo().slug}
                      canDelete={props.canDelete?.() === true}
                      deleting={deletingID() === source.id}
                      onDelete={() => handleDelete(source)}
                    />
                  )}
                </For>
              </div>
            </Show>
          </Show>
        </Show>
      </Card>
    </div>
  );
}

function SourceRow(props) {
  const source = () => props.source;
  const detailHref = () => `/${props.slug}/sources/${source().id}`;
  return (
    <div class="border rounded dark:border-gray-700 hover:border-blue-400 dark:hover:border-blue-500 transition">
      <div class="flex items-center justify-between p-3 gap-3">
        <A
          href={detailHref()}
          class="min-w-0 flex-1 group"
        >
          <p
            class="text-blue-600 dark:text-blue-400 group-hover:underline text-sm font-medium block truncate"
            title={source().url}
          >
            {source().parsed_title && source().parsed_title.trim().length > 0
              ? source().parsed_title
              : source().url}
          </p>
          <Show when={source().parsed_title && source().parsed_title.trim().length > 0}>
            <p class="text-xs text-gray-500 dark:text-gray-400 truncate" title={source().url}>
              {source().url}
            </p>
          </Show>
          <div class="flex items-center gap-2 mt-1 flex-wrap text-xs text-gray-500 dark:text-gray-400">
            <Badge variant={statusVariant(source().status)}>{source().status}</Badge>
            <Show when={source().parse_status}>
              <Badge variant={
                source().parse_status === "ok" ? "green"
                : source().parse_status === "failed" ? "red"
                : "yellow"
              }>
                {source().parse_status}
              </Badge>
            </Show>
            <Show when={source().kind}>
              <Badge variant="gray">{source().kind}</Badge>
            </Show>
            <Show when={source().fetched_at && source().fetched_at.Valid}>
              <span>fetched {formatTimestamp(source().fetched_at)}</span>
            </Show>
            <Show when={source().error}>
              <span class="text-red-600 dark:text-red-400" title={source().error}>
                {source().error.length > 80
                  ? source().error.slice(0, 80) + "..."
                  : source().error}
              </span>
            </Show>
          </div>
        </A>
        <Show when={props.canDelete}>
          <Button
            variant="danger"
            onClick={props.onDelete}
            loading={props.deleting}
            loadingText="Deleting..."
            class="text-xs px-2 py-1"
          >
            Delete
          </Button>
        </Show>
      </div>
    </div>
  );
}
