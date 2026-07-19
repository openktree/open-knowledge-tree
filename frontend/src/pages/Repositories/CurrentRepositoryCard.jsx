import { Show } from "solid-js";
import Button from "../../components/Button";
import Card from "../../components/Card";
import { useRepository } from "../../store/repository";

// CurrentRepositoryCard is the card pinned to the top of the
// Repositories page showing the active repository, with a Copy ID
// affordance. Split out of index.jsx to satisfy the page-size
// policy (index.jsx had an internal subcomponent).
//
// Props:
//   - onAlert: (alert) => void
export default function CurrentRepositoryCard(props) {
  const repo = useRepository();

  const copyID = async () => {
    const id = repo.currentRepo() ? repo.currentRepo().id : "";
    if (!id) return;
    try {
      await navigator.clipboard.writeText(id);
      props.onAlert?.({ variant: "success", message: "Repository ID copied" });
    } catch {
      props.onAlert?.({ variant: "error", message: "Could not copy to clipboard" });
    }
  };

  return (
    <Card>
      <div class="flex items-start justify-between gap-4">
        <div class="min-w-0 flex-1">
          <h2 class="text-lg font-semibold dark:text-white">Current Repository</h2>
          <Show
            when={repo.currentRepo()}
            fallback={
              <p class="text-sm text-gray-500 dark:text-gray-400 mt-1">
                No repository is selected. Every page in the application operates inside a
                repository, so create or select one below to start.
              </p>
            }
          >
            <p class="text-xl font-semibold mt-1 dark:text-gray-100 truncate">
              {repo.currentRepo().name}
            </p>
            <p class="text-xs text-gray-500 dark:text-gray-400 font-mono truncate">
              {repo.currentRepo().id}
            </p>
            <Show when={repo.currentRepo().description}>
              <p class="text-sm text-gray-600 dark:text-gray-300 mt-2">
                {repo.currentRepo().description}
              </p>
            </Show>
          </Show>
        </div>
        <Show when={repo.currentRepo()}>
          <Button variant="secondary" onClick={copyID}>
            Copy ID
          </Button>
        </Show>
      </div>
    </Card>
  );
}
