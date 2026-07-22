import { useLocation, useNavigate } from "@solidjs/router";
import { createResource, createSignal, For, onCleanup, onMount, Show } from "solid-js";
import { api } from "../services/api";
import { getTokenSignal, setToken } from "../store/auth";
import { useRBAC } from "../store/rbac";
import { useRepository } from "../store/repository";
import { useTheme } from "../store/theme";

// UserMenu is the top-right account dropdown. Shows the current user's
// email (fetched lazily from /users/me) + a chevron; the menu offers
// "Profile" (→ /profile, where API tokens live) and "Sign Out" (the
// existing handleLogout). Mirrors RepositoryDropdown's click-outside
// pattern so the menu closes on outside clicks and Escape.
function UserMenu(props) {
  const [open, setOpen] = createSignal(false);
  let menuRef;

  const handleClickOutside = (e) => {
    if (menuRef && !menuRef.contains(e.target)) setOpen(false);
  };
  const handleKey = (e) => {
    if (e.key === "Escape") setOpen(false);
  };
  onMount(() => {
    document.addEventListener("mousedown", handleClickOutside);
    document.addEventListener("keydown", handleKey);
  });
  onCleanup(() => {
    document.removeEventListener("mousedown", handleClickOutside);
    document.removeEventListener("keydown", handleKey);
  });

  const goProfile = () => {
    setOpen(false);
    props.onNavigate?.("/profile");
  };
  const goTokens = () => {
    setOpen(false);
    props.onNavigate?.("/profile?tab=tokens");
  };
  const signOut = () => {
    setOpen(false);
    props.onSignOut?.();
  };

  const email = () => props.user()?.email || "Account";
  const displayName = () => props.user()?.display_name || "";

  return (
    <div class="relative" ref={menuRef}>
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        class="flex items-center gap-2 text-sm border border-border rounded-md px-2.5 py-1.5 bg-surface text-text-muted hover:bg-primary-soft transition max-w-[200px]"
        title="Account"
      >
        <svg
          xmlns="http://www.w3.org/2000/svg"
          class="h-4 w-4 flex-shrink-0 text-text-muted"
          viewBox="0 0 20 20"
          fill="currentColor"
        >
          <path d="M10 9a3 3 0 100-6 3 3 0 000 6zm-7 9a7 7 0 1114 0H3z" />
        </svg>
        <span class="truncate font-medium hidden sm:inline">{email()}</span>
        <svg
          xmlns="http://www.w3.org/2000/svg"
          class="h-3.5 w-3.5 flex-shrink-0 text-text-muted transition-transform"
          classList={{ "rotate-180": open() }}
          viewBox="0 0 20 20"
          fill="currentColor"
        >
          <path
            fill-rule="evenodd"
            d="M5.23 7.21a.75.75 0 011.06.02L10 11.06l3.71-3.83a.75.75 0 111.08 1.04l-4.25 4.39a.75.75 0 01-1.08 0L5.21 8.27a.75.75 0 01.02-1.06z"
            clip-rule="evenodd"
          />
        </svg>
      </button>
      <Show when={open()}>
        <div class="absolute right-0 mt-2 w-56 bg-surface border border-border rounded-md shadow-lg z-20 overflow-hidden">
          <div class="px-3 py-2 border-b border-border">
            <div class="text-sm font-medium text-text-base truncate">{displayName()}</div>
            <div class="text-xs text-text-muted truncate">{email()}</div>
          </div>
          <button
            type="button"
            onClick={goProfile}
            class="w-full text-left px-3 py-2 text-sm text-text-base hover:bg-primary-soft transition flex items-center gap-2"
          >
            <svg
              xmlns="http://www.w3.org/2000/svg"
              class="h-4 w-4 text-text-muted"
              viewBox="0 0 20 20"
              fill="currentColor"
            >
              <path
                fill-rule="evenodd"
                d="M10 9a3 3 0 100-6 3 3 0 000 6zm-7 9a7 7 0 1114 0H3z"
                clip-rule="evenodd"
              />
            </svg>
            Profile
          </button>
          <button
            type="button"
            onClick={goTokens}
            class="w-full text-left px-3 py-2 text-sm text-text-base hover:bg-primary-soft transition flex items-center gap-2"
          >
            <svg
              xmlns="http://www.w3.org/2000/svg"
              class="h-4 w-4 text-text-muted"
              viewBox="0 0 20 20"
              fill="currentColor"
            >
              <path
                fill-rule="evenodd"
                d="M11.49 3.17a1 1 0 011.02 1.07 1 1 0 01-.05.27l-3.5 9a1 1 0 11-1.88-.66l3.5-9a1 1 0 01.91-.68zM5.7 7.3a1 1 0 010 1.4L3.4 11l2.3 2.3a1 1 0 11-1.4 1.4l-3-3a1 1 0 010-1.4l3-3a1 1 0 011.4 0zm8.6 0a1 1 0 011.4 0l3 3a1 1 0 010 1.4l-3 3a1 1 0 11-1.4-1.4l2.3-2.3-2.3-2.3a1 1 0 010-1.4z"
                clip-rule="evenodd"
              />
            </svg>
            API Tokens
          </button>
          <button
            type="button"
            onClick={signOut}
            class="w-full text-left px-3 py-2 text-sm text-danger hover:bg-danger/10 transition flex items-center gap-2"
          >
            <svg
              xmlns="http://www.w3.org/2000/svg"
              class="h-4 w-4"
              viewBox="0 0 20 20"
              fill="currentColor"
            >
              <path
                fill-rule="evenodd"
                d="M3 3a1 1 0 011-1h7a1 1 0 110 2H5v12h6a1 1 0 110 2H4a1 1 0 01-1-1V3zm10.293 4.293a1 1 0 011.414 0L17.414 10l-2.707 2.707a1 1 0 01-1.414-1.414L14.586 10l-1.293-1.293a1 1 0 010-1.414z"
                clip-rule="evenodd"
              />
            </svg>
            Sign Out
          </button>
        </div>
      </Show>
    </div>
  );
}

function RepositoryDropdown() {
  const { repositories, currentRepo, selectRepository } = useRepository();
  const navigate = useNavigate();
  const location = useLocation();
  const [open, setOpen] = createSignal(false);
  let menuRef;

  const handleSelect = (repo) => {
    selectRepository(repo);
    setOpen(false);
    const segments = location.pathname.split("/").filter(Boolean);
    const currentSlug = currentRepo()?.slug;
    if (currentSlug && segments[0] === currentSlug) {
      segments[0] = repo.slug;
      navigate("/" + segments.join("/"), { replace: true });
    }
  };

  const handleClickOutside = (e) => {
    if (menuRef && !menuRef.contains(e.target)) {
      setOpen(false);
    }
  };

  onMount(() => {
    document.addEventListener("mousedown", handleClickOutside);
  });

  onCleanup(() => {
    document.removeEventListener("mousedown", handleClickOutside);
  });

  return (
    <div class="relative" ref={menuRef}>
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        class="flex items-center gap-2 text-sm border border-border rounded-md px-2.5 py-1.5 bg-surface text-text-muted hover:bg-primary-soft transition max-w-[180px]"
        title="Switch active repository"
      >
        <svg
          xmlns="http://www.w3.org/2000/svg"
          class="h-4 w-4 flex-shrink-0 text-text-muted"
          viewBox="0 0 20 20"
          fill="currentColor"
        >
          <path d="M2 5a2 2 0 012-2h4a2 2 0 012 2v1H4a1 1 0 00-1 1v1h11V6a1 1 0 00-1-1h-1V5a2 2 0 012-2h2a2 2 0 012 2v9a2 2 0 01-2 2H4a2 2 0 01-2-2V5z" />
        </svg>
        <span class="truncate font-medium">
          {currentRepo() ? currentRepo().name : "Select repository"}
        </span>
        <svg
          xmlns="http://www.w3.org/2000/svg"
          class="h-3.5 w-3.5 flex-shrink-0 text-text-muted transition-transform"
          classList={{ "rotate-180": open() }}
          viewBox="0 0 20 20"
          fill="currentColor"
        >
          <path
            fill-rule="evenodd"
            d="M5.23 7.21a.75.75 0 011.06.02L10 11.06l3.71-3.83a.75.75 0 111.08 1.04l-4.25 4.39a.75.75 0 01-1.08 0L5.21 8.27a.75.75 0 01.02-1.06z"
            clip-rule="evenodd"
          />
        </svg>
      </button>

      <Show when={open()}>
        <div class="absolute right-0 mt-2 w-64 bg-surface border border-border rounded-md shadow-lg z-20 overflow-hidden">
          <div class="px-3 py-2 text-xs font-semibold uppercase tracking-wide text-text-muted border-b border-border">
            Switch repository
          </div>
          <ul class="max-h-72 overflow-y-auto py-1">
            <For
              each={repositories()}
              fallback={
                <li class="px-3 py-3 text-sm text-text-muted">No repositories available</li>
              }
            >
              {(repo) => {
                const isActive = () => currentRepo()?.id === repo.id;
                return (
                  <li>
                    <button
                      type="button"
                      onClick={() => handleSelect(repo)}
                      class="w-full text-left px-3 py-2 flex items-center gap-2 hover:bg-primary-soft transition"
                      classList={{
                        "bg-primary-soft": isActive(),
                      }}
                    >
                      <svg
                        xmlns="http://www.w3.org/2000/svg"
                        class="h-4 w-4 flex-shrink-0"
                        classList={{
                          "text-primary-fg": isActive(),
                          "text-text-muted": !isActive(),
                        }}
                        viewBox="0 0 20 20"
                        fill="currentColor"
                      >
                        <path d="M2 5a2 2 0 012-2h4a2 2 0 012 2v1H4a1 1 0 00-1 1v1h11V6a1 1 0 00-1-1h-1V5a2 2 0 012-2h2a2 2 0 012 2v9a2 2 0 01-2 2H4a2 2 0 01-2-2V5z" />
                      </svg>
                      <span class="flex-1 min-w-0">
                        <span
                          class="block text-sm truncate"
                          classList={{
                            "font-semibold text-primary-fg": isActive(),
                            "text-text-base": !isActive(),
                          }}
                        >
                          {repo.name}
                        </span>
                        <Show when={repo.description}>
                          <span class="block text-xs text-text-muted truncate">
                            {repo.description}
                          </span>
                        </Show>
                      </span>
                      <Show when={isActive()}>
                        <svg
                          xmlns="http://www.w3.org/2000/svg"
                          class="h-4 w-4 text-primary-fg flex-shrink-0"
                          viewBox="0 0 20 20"
                          fill="currentColor"
                        >
                          <path
                            fill-rule="evenodd"
                            d="M16.704 5.29a1 1 0 010 1.42l-7.5 7.5a1 1 0 01-1.42 0l-3.5-3.5a1 1 0 011.42-1.42L8.5 12.09l6.79-6.8a1 1 0 011.414 0z"
                            clip-rule="evenodd"
                          />
                        </svg>
                      </Show>
                    </button>
                  </li>
                );
              }}
            </For>
          </ul>
        </div>
      </Show>
    </div>
  );
}

export default function Layout(props) {
  const navigate = useNavigate();
  const location = useLocation();
  const rbac = useRBAC();
  const { repositories, currentRepo, selectRepository } = useRepository();
  const { theme, toggle } = useTheme();
  const [collapsed, setCollapsed] = createSignal(localStorage.getItem("navbar_collapsed") === "1");
  const toggleCollapsed = () => {
    setCollapsed((c) => {
      const next = !c;
      localStorage.setItem("navbar_collapsed", next ? "1" : "0");
      return next;
    });
  };

  // Lazily fetch the current user (for the header dropdown label +
  // the Profile page). Mirrors Dashboard's pattern: createResource
  // keyed on the token signal so a login/logout re-fetches.
  const [currentUser] = createResource(getTokenSignal(), (t) => (t ? api.getMe() : null));

  const handleLogout = async () => {
    try {
      await api.logout();
    } catch {
    } finally {
      setToken(null);
      localStorage.removeItem("repository_id");
      navigate("/login", { replace: true });
    }
  };

  const showSources = () => rbac.hasPermission("source", "read");
  const showFacts = () => rbac.hasPermission("fact", "read");
  const showConcepts = () => rbac.hasPermission("concept", "read");
  const showInvestigations = () => rbac.hasPermission("investigation", "read");
  const showReports = () => rbac.hasPermission("report", "read");
  const showProviders = () =>
    rbac.hasPermission("source_provider", "read") || rbac.hasPermission("ai_provider", "read");
  const showTasks = () => rbac.hasPermission("task", "read");
  const showAIUsage = () => rbac.hasPermission("ai_usage", "read");
  const showAudit = () => rbac.hasPermission("audit", "read");
  // System-scope Tasks / AI Usage pages aggregate across every
  // repository, so only sysadmins reach them. Repo-scoped
  // equivalents live under /:slug/tasks and /:slug/ai-usage for
  // repoadmin (and sysadmin). The system links are hidden for
  // non-sysadmins via systemAdmin() so the page doesn't render a
  // 403 when clicked.
  const showSystemTasks = () => rbac.systemAdmin();
  const showSystemAIUsage = () => rbac.systemAdmin();
  const showSystemAudit = () => rbac.systemAdmin();
  const showUsers = () => rbac.hasPermission("user", "read");
  const showRemote = () => rbac.hasPermission("remote", "read");
  const showRepositories = () =>
    rbac.hasPermission("repository", "read") ||
    rbac.hasPermission("repository", "write") ||
    rbac.hasPermission("repository", "update") ||
    rbac.hasPermission("repository", "delete");

  const repoID = () => currentRepo()?.id;

  const navIcon = {
    "/dashboard":
      "M3 12l2-2m0 0l7-7 7 7M5 10v10a1 1 0 001 1h3m10-11l2 2m-2-2v10a1 1 0 01-1 1h-3m-6 0a1 1 0 001-1v-4a1 1 0 011-1h2a1 1 0 011 1v4a1 1 0 001 1m-6 0h6",
    "/investigations": "M21 21l-6-6m2-5a7 7 0 11-14 0 7 7 0 0114 0z",
    "/sources":
      "M19 20H5a2 2 0 01-2-2V6a2 2 0 012-2h10a2 2 0 012 2v1m2 13a2 2 0 01-2-2V7m2 13a2 2 0 002-2V9a2 2 0 00-2-2h-2m-4-3H9M7 16h6M7 8h6v4H7V8z",
    "/facts":
      "M9 12h6m-6 4h6m2 5H7a2 2 0 01-2-2V5a2 2 0 012-2h5.586a1 1 0 01.707.293l5.414 5.414a1 1 0 01.293.707V19a2 2 0 01-2 2z",
    "/concepts":
      "M7 7h.01M7 3h5c.512 0 1.024.195 1.414.586l7 7a2 2 0 010 2.828l-7 7a2 2 0 01-2.828 0l-7-7A1.984 1.984 0 013 12V7a4 4 0 014-4z",
    "/reports":
      "M9 17v-2m3 2v-4m3 4v-6m2 10H7a2 2 0 01-2-2V5a2 2 0 012-2h5.586a1 1 0 01.707.293l5.414 5.414a1 1 0 01.293.707V19a2 2 0 01-2 2z",
    "/providers":
      "M12 6V4m0 2a2 2 0 100 4m0-4a2 2 0 110 4m-6 8a2 2 0 100-4m0 4a2 2 0 110-4m0 4v2m0-6V4m6 6v10m6-2a2 2 0 100-4m0 4a2 2 0 110-4m0 4v2m0-6V4",
    "/remote":
      "M15 17h5l-1.405-1.405A2.032 2.032 0 0118 14.158V11a6.002 6.002 0 00-4-5.659V5a2 2 0 10-4 0v.341C7.67 6.165 6 8.388 6 11v3.159c0 .538-.214 1.055-.595 1.436L4 17h5m6 0v1a3 3 0 11-6 0v-1m6 0H9",
    "/users":
      "M17 20h5v-2a3 3 0 00-5.356-1.857M17 20H7m10 0v-2c0-.656-.126-1.283-.356-1.857M7 20H2v-2a3 3 0 015.356-1.857M7 20v-2c0-.656.126-1.283.356-1.857m0 0a5.002 5.002 0 019.288 0M15 7a3 3 0 11-6 0 3 3 0 016 0zm6 3a2 2 0 11-4 0 2 2 0 014 0zM7 10a2 2 0 11-4 0 2 2 0 014 0z",
    "/tasks":
      "M9 5H7a2 2 0 00-2 2v12a2 2 0 002 2h10a2 2 0 002-2V7a2 2 0 00-2-2h-2M9 5a2 2 0 002 2h2a2 2 0 002-2M9 5a2 2 0 012-2h2a2 2 0 012 2m-6 9l2 2 4-4",
    "/ai-usage": "M13 10V3L4 14h7v7l9-11h-7z",
    "/audit":
      "M9 12h6m-6 4h6m2 5H7a2 2 0 01-2-2V5a2 2 0 012-2h5.586a1 1 0 01.707.293l5.414 5.414a1 1 0 01.293.707V19a2 2 0 01-2 2z",
    "/repositories":
      "M19 11H5m14 0a2 2 0 012 2v6a2 2 0 01-2 2H5a2 2 0 01-2-2v-6a2 2 0 012-2m7 0V5a2 2 0 012-2h2a2 2 0 012 2v2a2 2 0 01-2 2h-2a2 2 0 01-2-2z",
  };
  const settingsIcon =
    "M10.325 4.317c.426-1.756 2.924-1.756 3.35 0a1.724 1.724 0 002.573 1.066c1.543-.94 3.31.826 2.37 2.37a1.724 1.724 0 001.065 2.572c1.756.426 1.756 2.924 0 3.35a1.724 1.724 0 00-1.066 2.573c.94 1.543-.826 3.31-2.37 2.37a1.724 1.724 0 00-2.572 1.065c-.426 1.756-2.924 1.756-3.35 0a1.724 1.724 0 00-2.573-1.066c-1.543.94-3.31-.826-2.37-2.37a1.724 1.724 0 00-1.065-2.572c-1.756-.426-1.756-2.924 0-3.35a1.724 1.724 0 001.066-2.573c-.94-1.543.826-3.31 2.37-2.37.996.608 2.296.07 2.572-1.065zM15 12a3 3 0 11-6 0 3 3 0 016 0z";

  const NavLink = (props) => {
    const icon = navIcon[props.href] || settingsIcon;
    return (
      <a
        href={props.href}
        class="text-sm px-3 py-2 rounded transition flex items-center gap-2"
        classList={{
          "text-primary-fg bg-primary-soft font-medium": location.pathname === props.href,
          "text-text-muted hover:text-text-base": location.pathname !== props.href,
          "justify-center": collapsed(),
        }}
        title={collapsed() ? props.children : undefined}
      >
        <svg
          xmlns="http://www.w3.org/2000/svg"
          class="h-4 w-4 flex-shrink-0"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="2"
          stroke-linecap="round"
          stroke-linejoin="round"
        >
          <path d={icon} />
        </svg>
        <Show when={!collapsed()}>
          <span class="truncate">{props.children}</span>
        </Show>
      </a>
    );
  };

  const SettingsLink = (props) => {
    const href = `/repositories/${props.repoID}/settings`;
    return (
      <a
        href={href}
        class="text-sm px-3 py-2 rounded transition flex items-center gap-2"
        classList={{
          "text-primary-fg bg-primary-soft font-medium": location.pathname === href,
          "text-text-muted hover:text-text-base": location.pathname !== href,
          "justify-center": collapsed(),
        }}
        title={collapsed() ? "Settings" : undefined}
      >
        <svg
          xmlns="http://www.w3.org/2000/svg"
          class="h-4 w-4 flex-shrink-0"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="2"
          stroke-linecap="round"
          stroke-linejoin="round"
        >
          <path d={settingsIcon} />
        </svg>
        <Show when={!collapsed()}>
          <span>Settings</span>
        </Show>
      </a>
    );
  };

  const SectionHeader = (props) => (
    <Show when={!collapsed()} fallback={<div class="pt-4 pb-1 mx-3 border-t border-border" />}>
      <div class="px-3 pt-4 pb-1 text-[11px] font-semibold uppercase tracking-wider text-text-muted">
        {props.children}
      </div>
    </Show>
  );

  const Divider = () => <div class="my-2 mx-3 border-t border-border" />;

  const showSettings = () =>
    rbac.hasPermission("repository", "write") || rbac.hasPermission("repository", "update");

  return (
    <div class="h-screen bg-page transition-colors flex overflow-hidden">
      <aside
        class="bg-surface border-r border-border flex-shrink-0 transition-all duration-200 flex flex-col h-full"
        classList={{ "w-56": !collapsed(), "w-16": collapsed() }}
      >
        <div
          class="px-3 py-3 border-b border-border flex-shrink-0 flex items-center"
          classList={{ "justify-end": collapsed(), "justify-between": !collapsed() }}
        >
          <Show when={!collapsed()}>
            <h1 class="text-base font-bold text-text-base truncate">Open Knowledge Tree</h1>
          </Show>
          <button
            type="button"
            onClick={toggleCollapsed}
            class="p-1.5 rounded text-text-muted hover:bg-primary-soft hover:text-text-base transition flex-shrink-0"
            title={collapsed() ? "Expand sidebar" : "Collapse sidebar"}
          >
            <svg
              xmlns="http://www.w3.org/2000/svg"
              class="h-4 w-4 transition-transform duration-200"
              classList={{ "rotate-180": collapsed() }}
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="2"
              stroke-linecap="round"
              stroke-linejoin="round"
            >
              <path d="M11 19l-7-7 7-7m8 14l-7-7 7-7" />
            </svg>
          </button>
        </div>
        <nav
          class="flex flex-col p-2 gap-1 overflow-y-auto flex-1 min-h-0"
          classList={{ "p-3": !collapsed() }}
        >
          <SectionHeader>Workspace</SectionHeader>
          <NavLink href="/dashboard">Dashboard</NavLink>
          <Show when={showInvestigations()}>
            <NavLink href="/investigations">Investigations</NavLink>
          </Show>
          <Show when={showSources()}>
            <NavLink href="/sources">Sources</NavLink>
          </Show>
          <Show when={showFacts()}>
            <NavLink href="/facts">Facts</NavLink>
          </Show>
          <Show when={showConcepts()}>
            <NavLink href="/concepts">Concepts</NavLink>
          </Show>
          <Show when={showReports()}>
            <NavLink href="/reports">Reports</NavLink>
          </Show>

          <div class="mt-auto flex flex-col gap-1">
            <Show
              when={
                showProviders() ||
                showSettings() ||
                showRemote() ||
                showUsers() ||
                showTasks() ||
                showAIUsage() ||
                showAudit()
              }
            >
              <Divider />
              <SectionHeader>Repository</SectionHeader>
              <Show when={showProviders()}>
                <NavLink href="/providers">Providers</NavLink>
              </Show>
              <Show when={showSettings()}>
                <Show
                  when={repoID()}
                  fallback={
                    <Show when={!collapsed()}>
                      <span class="block text-sm px-3 py-2 text-text-muted">Settings</span>
                    </Show>
                  }
                >
                  <SettingsLink repoID={repoID()} />
                </Show>
              </Show>
              <Show when={showRemote()}>
                <NavLink href="/remote">Remote</NavLink>
              </Show>
              <NavLink href="/promptsets">Promptsets</NavLink>
              <Show when={showUsers()}>
                <NavLink href="/users">Users</NavLink>
              </Show>
              <Show when={showTasks() && repoID()}>
                <NavLink href={`/${currentRepo()?.slug}/tasks`}>Tasks</NavLink>
              </Show>
              <Show when={showAIUsage() && repoID()}>
                <NavLink href={`/${currentRepo()?.slug}/ai-usage`}>AI Usage</NavLink>
              </Show>
              <Show when={showAudit() && repoID()}>
                <NavLink href={`/repositories/${currentRepo()?.slug}/audit`}>Audit Log</NavLink>
              </Show>
            </Show>

            <Show
              when={
                showRepositories() || showSystemTasks() || showSystemAIUsage() || showSystemAudit()
              }
            >
              <Divider />
              <SectionHeader>System</SectionHeader>
              <Show when={showRepositories()}>
                <NavLink href="/repositories">Repositories</NavLink>
              </Show>
              <Show when={showSystemTasks()}>
                <NavLink href="/tasks">Tasks</NavLink>
              </Show>
              <Show when={showSystemAIUsage()}>
                <NavLink href="/ai-usage">AI Usage</NavLink>
              </Show>
              <Show when={showSystemAudit()}>
                <NavLink href="/audit">Audit Log</NavLink>
              </Show>
            </Show>
          </div>
        </nav>
      </aside>

      <div class="flex-1 flex flex-col min-w-0 h-full overflow-hidden">
        <header class="bg-surface border-b border-border transition-colors flex-shrink-0">
          <div class="px-6 py-3 flex items-center justify-end gap-4">
            <Show when={repositories().length > 0}>
              <RepositoryDropdown />
            </Show>

            <button
              onClick={toggle}
              class="p-2 rounded text-text-muted hover:bg-primary-soft hover:text-text-base transition"
              title={theme() === "dark" ? "Switch to light mode" : "Switch to dark mode"}
            >
              <Show
                when={theme() === "dark"}
                fallback={
                  <svg
                    xmlns="http://www.w3.org/2000/svg"
                    class="h-5 w-5"
                    viewBox="0 0 20 20"
                    fill="currentColor"
                  >
                    <path
                      fill-rule="evenodd"
                      d="M10 2a1 1 0 011 1v1a1 1 0 11-2 0V3a1 1 0 011-1zm4 8a4 4 0 11-8 0 4 4 0 018 0zm-.464 4.95l.707.707a1 1 0 001.414-1.414l-.707-.707a1 1 0 00-1.414 1.414zm2.12-10.607a1 1 0 010 1.414l-.706.707a1 1 0 11-1.414-1.414l.707-.707a1 1 0 011.414 0zM17 11a1 1 0 100-2h-1a1 1 0 100 2h1zm-7 4a1 1 0 011 1v1a1 1 0 11-2 0v-1a1 1 0 011-1zM5.05 6.464A1 1 0 106.465 5.05l-.708-.707a1 1 0 00-1.414 1.414l.707.707zm1.414 8.486l-.707.707a1 1 0 01-1.414-1.414l.707-.707a1 1 0 011.414 1.414zM4 11a1 1 0 100-2H3a1 1 0 000 2h1z"
                      clip-rule="evenodd"
                    />
                  </svg>
                }
              >
                <svg
                  xmlns="http://www.w3.org/2000/svg"
                  class="h-5 w-5"
                  viewBox="0 0 20 20"
                  fill="currentColor"
                >
                  <path d="M17.293 13.293A8 8 0 016.707 2.707a8.001 8.001 0 1010.586 10.586z" />
                </svg>
              </Show>
            </button>

            <UserMenu
              user={currentUser}
              onNavigate={(path) => navigate(path)}
              onSignOut={handleLogout}
            />
          </div>
        </header>

        <main
          class={`flex-1 w-full mx-auto px-6 py-8 overflow-y-auto ${props.maxWidth || "max-w-6xl"}`}
        >
          {props.children}
        </main>
      </div>
    </div>
  );
}
