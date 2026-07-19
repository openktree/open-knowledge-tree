import { createMemo, createResource, Show } from "solid-js";
import Layout from "../../components/Layout";
import Loading from "../../components/Loading";
import { api } from "../../services/api";
import { getTokenSignal } from "../../store/auth";
import { useRBAC } from "../../store/rbac";
import { useRepository } from "../../store/repository";
import ActionGrid from "./ActionGrid";
import { ACTION_CARDS } from "./constants";
import RegistryBanner from "./RegistryBanner";
import StatGrid from "./StatGrid";

export default function Dashboard() {
  const token = getTokenSignal();
  const rbac = useRBAC();
  const repo = useRepository();

  const [user] = createResource(token, (t) => (t ? api.getMe() : null));

  const slug = () => repo.currentRepo()?.slug || "";
  const repoName = () => repo.currentRepo()?.name || "No repository selected";

  const [stats] = createResource(
    () => ({ slug: slug(), ready: !!user() && !!slug() }),
    async ({ slug, ready }) => {
      if (!ready || !slug) return null;
      const [sources, facts, concepts] = await Promise.allSettled([
        api.listSources(slug, { limit: 1 }),
        api.listRepoFacts(slug, "stable", "", { limit: 1 }),
        api.listRepoConcepts(slug, { limit: 1 }),
      ]);
      const pick = (r) => (r.status === "fulfilled" ? (r.value?.total ?? 0) : "\u2014");
      return {
        sourceCount: pick(sources),
        factCount: pick(facts),
        conceptCount: pick(concepts),
      };
    },
  );

  const visibleCards = createMemo(() => {
    const cards = [];
    if (rbac.hasPermission("investigation", "read")) cards.push(ACTION_CARDS[0]);
    if (rbac.hasPermission("report", "read")) cards.push(ACTION_CARDS[1]);
    if (rbac.hasPermission("concept", "read")) cards.push(ACTION_CARDS[2]);
    return cards;
  });

  return (
    <Show when={user()} fallback={<Loading />}>
      <Layout>
        <div class="space-y-6">
          <div>
            <h2 class="text-2xl font-semibold dark:text-white">
              Welcome, {user().display_name || user().email}
            </h2>
            <p class="text-gray-500 dark:text-gray-400 mt-1">
              Here is an overview of your workspace.
            </p>
          </div>

          <Show when={slug()}>
            <RegistryBanner repoID={() => repo.currentRepo()?.id} />
          </Show>

          <Show
            when={slug()}
            fallback={
              <div class="bg-white dark:bg-gray-800 rounded-lg shadow-md p-8 text-center">
                <p class="text-gray-500 dark:text-gray-400 text-sm">
                  No repository selected. Pick one from the dropdown above to see your stats.
                </p>
              </div>
            }
          >
            <Show when={!stats.loading} fallback={<Loading message="Loading stats..." />}>
              <StatGrid
                repoName={repoName()}
                sourceCount={stats()?.sourceCount ?? "\u2014"}
                factCount={stats()?.factCount ?? "\u2014"}
                conceptCount={stats()?.conceptCount ?? "\u2014"}
              />
            </Show>
          </Show>

          <Show when={visibleCards().length > 0}>
            <ActionGrid cards={visibleCards()} />
          </Show>
        </div>
      </Layout>
    </Show>
  );
}
