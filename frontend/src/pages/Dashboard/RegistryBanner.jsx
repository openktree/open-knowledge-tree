import { createMemo, createResource, Show } from "solid-js";
import { api } from "../../services/api";
import { useRBAC } from "../../store/rbac";

// RegistryBanner surfaces the per-repo registry integration status
// on the dashboard and investigations pages. Collapsed by default
// to a single line; hover/focus expands the full explanation so the
// banner doesn't crowd the page but stays discoverable.
//
// Three states:
//   1. registry_configured && registry_enabled && auto_contribute
//      → "Sharing enabled" (blue): sources and facts are shared
//        publicly with other researchers via the remote registry.
//   2. registry_configured && registry_enabled && !auto_contribute
//      → "Using remote registry as cache" (indigo) with a
//        suggestion to enable auto-contribute (link to settings).
//   3. !registry_configured || !registry_enabled
//      → no banner (the integration is off for this repo).
//
// Props:
//   - repoID: () => string
//   - canManage: () => boolean  (optional — controls whether the
//     settings link is rendered as a button vs a plain hint)
export default function RegistryBanner(props) {
  const rbac = useRBAC();
  const [settings] = createResource(props.repoID, async (id) => {
    if (!id) return null;
    try {
      return await api.getRepositorySettings(id);
    } catch {
      return null;
    }
  });

  const canManage = createMemo(
    () => props.canManage?.() ?? rbac.hasPermission("repository", "manage"),
  );

  const sharing = () => {
    const s = settings();
    return !!s && s.registry_configured && s.registry_enabled && s.auto_contribute;
  };
  const cacheOnly = () => {
    const s = settings();
    return !!s && s.registry_configured && s.registry_enabled && !s.auto_contribute;
  };

  // group/peer: a <details> element gives us the collapsed-title +
  // hover/focus-to-expand behavior with zero JS. The title line is
  // the <summary>; the expanded body is everything else.
  return (
    <Show when={sharing() || cacheOnly()}>
      <details
        class={`group rounded-lg border shadow-sm transition-shadow hover:shadow-md focus-within:shadow-md ${
          sharing()
            ? "bg-blue-50 dark:bg-blue-900/50 border-blue-200 dark:border-blue-700"
            : "bg-indigo-50 dark:bg-indigo-900/50 border-indigo-200 dark:border-indigo-700"
        }`}
      >
        <summary
          class="flex items-center gap-2.5 cursor-pointer list-none p-3 select-none"
          tabindex="0"
        >
          <svg
            xmlns="http://www.w3.org/2000/svg"
            class={`h-4 w-4 flex-shrink-0 ${
              sharing()
                ? "text-blue-600 dark:text-blue-400"
                : "text-indigo-600 dark:text-indigo-400"
            }`}
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            stroke-width="2"
            stroke-linecap="round"
            stroke-linejoin="round"
          >
            <path d="M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
            <path d="M12 8v4l3 3" />
          </svg>
          <span
            class={`text-sm font-medium ${
              sharing()
                ? "text-blue-900 dark:text-blue-200"
                : "text-indigo-900 dark:text-indigo-200"
            }`}
          >
            <Show when={sharing()}>Sharing is enabled — sources and facts are shared publicly</Show>
            <Show when={cacheOnly()}>Using a remote knowledge registry as a cache</Show>
          </span>
          {/* chevron rotates when open */}
          <svg
            class={`ml-auto h-4 w-4 transition-transform group-open:rotate-180 ${
              sharing()
                ? "text-blue-500 dark:text-blue-400"
                : "text-indigo-500 dark:text-indigo-400"
            }`}
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            stroke-width="2"
            stroke-linecap="round"
            stroke-linejoin="round"
          >
            <path d="M6 9l6 6 6-6" />
          </svg>
        </summary>
        <div class="px-3 pb-3 pt-0">
          <Show when={sharing()}>
            <p class="text-sm text-blue-800 dark:text-blue-300 leading-relaxed">
              This repository automatically pushes processed sources and their facts to the remote
              knowledge registry as soon as they finish processing. The retrieved sources and facts
              are shared publicly with other researchers, who can import them without running the
              decomposition pipeline again — saving compute and time. A repository admin can disable
              this in Settings.
            </p>
          </Show>
          <Show when={cacheOnly()}>
            <p class="text-sm text-indigo-800 dark:text-indigo-300 leading-relaxed">
              This repository pulls pre-decomposed sources (facts, concepts, embeddings) from the
              remote knowledge registry when available, skipping the AI pipeline. Sources you fetch
              are not shared back unless you enable auto-contribute.
            </p>
            <p class="mt-2 text-sm text-indigo-800 dark:text-indigo-300 leading-relaxed">
              By enabling auto-contribute, your decomposed sources are shared publicly with other
              researchers — they can import them without re-running the decomposition pipeline,
              saving compute and time for everyone working on the same material.
            </p>
            <div class="mt-2 text-sm text-indigo-800 dark:text-indigo-300">
              <Show
                when={canManage()}
                fallback={
                  <span>
                    Ask a repository admin to enable auto-contribute to share your sources and facts
                    publicly with other researchers.
                  </span>
                }
              >
                <span>
                  Want to give back? Enable auto-contribute in{" "}
                  <a
                    href={`#/repositories/${props.repoID()}/settings`}
                    class="underline font-medium hover:text-indigo-900 dark:hover:text-indigo-100"
                  >
                    Repository Settings
                  </a>{" "}
                  to share processed sources and facts publicly with other researchers.
                </span>
              </Show>
            </div>
          </Show>
        </div>
      </details>
    </Show>
  );
}
