import { useLocation } from "@solidjs/router";
import { createContext, createEffect, createResource, createSignal, useContext } from "solid-js";
import { api } from "../services/api";
import { getTokenSignal } from "./auth";

const defaults = {
  repositories: () => [],
  currentRepo: () => null,
  loaded: () => false,
  selectRepository: () => {},
  refresh: async () => {},
};

const RepositoryContext = createContext(defaults);

export function RepositoryProvider(props) {
  const [repositories, setRepositories] = createSignal([]);
  const [currentRepo, setCurrentRepo] = createSignal(null);
  const [loaded, setLoaded] = createSignal(false);

  const token = getTokenSignal();
  const location = useLocation();

  const [reposData] = createResource(token, async (t) => {
    if (!t) return null;
    try {
      const data = await api.listRepositories();
      return data;
    } catch {
      return { repositories: [] };
    }
  });

  createEffect(() => {
    const data = reposData();
    if (data) {
      setRepositories(data.repositories || []);

      const savedRepoId = localStorage.getItem("repository_id");
      if (savedRepoId && data.repositories) {
        const found = data.repositories.find((r) => r.id === savedRepoId);
        if (found) {
          setCurrentRepo(found);
        } else if (data.repositories.length > 0) {
          const first = data.repositories[0];
          setCurrentRepo(first);
          localStorage.setItem("repository_id", first.id);
        }
      } else if (data.repositories && data.repositories.length > 0) {
        const first = data.repositories[0];
        setCurrentRepo(first);
        localStorage.setItem("repository_id", first.id);
      }

      setLoaded(true);
    }
  });

  // Sync current repo from URL slug. The first path segment is
  // treated as a potential repository slug. When it matches a
  // repo in the list, that repo becomes current (URL takes
  // precedence over localStorage). This keeps the repo dropdown
  // and the X-Repository-ID header in sync with shareable URLs.
  createEffect(() => {
    const repos = repositories();
    if (repos.length === 0) return;
    const segments = location.pathname.split("/").filter(Boolean);
    const firstSegment = segments[0];
    if (!firstSegment) return;
    const matched = repos.find((r) => r.slug === firstSegment);
    if (matched && currentRepo()?.id !== matched.id) {
      setCurrentRepo(matched);
      localStorage.setItem("repository_id", matched.id);
    }
  });

  const selectRepository = (repo) => {
    setCurrentRepo(repo);
    localStorage.setItem("repository_id", repo.id);
  };

  const store = {
    repositories,
    currentRepo,
    loaded,
    selectRepository,
    refresh: async () => {
      try {
        const data = await api.listRepositories();
        setRepositories(data.repositories || []);
      } catch {}
    },
  };

  return <RepositoryContext.Provider value={store}>{props.children}</RepositoryContext.Provider>;
}

export function useRepository() {
  return useContext(RepositoryContext);
}
