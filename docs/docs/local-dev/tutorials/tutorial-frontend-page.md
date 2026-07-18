---
id: tutorial-frontend-page
sidebar_position: 4
title: Adding a Frontend Page
---

# Tutorial: Adding a Frontend Page

The frontend is a SolidJS SPA. This tutorial shows how to add a new page — for example, a "Bookmarks" page that lists saved facts.

## The page folder convention

Every page lives in `frontend/src/pages/<Name>/`:

```
frontend/src/pages/Bookmarks/
├── index.jsx          # Route entry — owns page-level state
├── BookmarksContent.jsx   # Main view (optional)
├── BookmarksTable.jsx     # Sub-component (optional)
└── constants.js           # Tab definitions, option maps (optional)
```

**Rules:**
- `index.jsx` is the only file other modules import (the router resolves `import Bookmarks from "./pages/Bookmarks"` to `pages/Bookmarks/index.jsx`).
- Keep sub-components (`BookmarksContent`, `BookmarksTable`) private to the folder.
- Page-level state (signals, resources) lives in `index.jsx`; sub-components receive props.
- Promote to `components/` only when reused across pages.

## Step 1: Create the page

Create `frontend/src/pages/Bookmarks/index.jsx`:

```jsx
import { createResource, Show, createSignal } from "solid-js";
import { api } from "../../services/api";
import { useRepository } from "../../store/repository";
import Layout from "../../components/Layout";
import Loading from "../../components/Loading";

export default function Bookmarks() {
  const repo = useRepository();
  const [search, setSearch] = createSignal("");

  const [bookmarks] = createResource(
    () => ({ slug: repo.currentRepo()?.slug || "", q: search() }),
    async ({ slug, q }) => {
      if (!slug) return null;
      return await api.listRepoFacts(slug, "stable", "created_at", { q, limit: 50 });
    }
  );

  return (
    <Layout>
      <div class="space-y-6">
        <h2 class="text-2xl font-semibold dark:text-white">Bookmarks</h2>
        <input
          type="text"
          placeholder="Search bookmarks..."
          class="border rounded px-3 py-2 dark:bg-gray-800 dark:text-white"
          onInput={(e) => setSearch(e.target.value)}
        />
        <Show when={!bookmarks.loading} fallback={<Loading />}>
          <Show
            when={bookmarks()?.facts?.length > 0}
            fallback={<p class="text-gray-500">No bookmarks found.</p>}
          >
            <div class="space-y-3">
              {bookmarks().facts.map((fact) => (
                <div class="bg-white dark:bg-gray-800 rounded-lg p-4 shadow">
                  <p class="text-sm dark:text-gray-200">{fact.text}</p>
                  <span class="text-xs text-gray-400">{fact.status}</span>
                </div>
              ))}
            </div>
          </Show>
        </Show>
      </div>
    </Layout>
  );
}
```

## Step 2: Add the route

In `frontend/src/App.jsx`, add the import and route:

```jsx
import Bookmarks from "./pages/Bookmarks";

// Inside the AuthGuard route:
<Route path="/bookmarks" component={Bookmarks} />
```

## Step 3: Add to the sidebar (optional)

If you want a sidebar link, add it to the sidebar component. The sidebar is typically defined in `frontend/src/components/Layout/Sidebar.jsx` or similar. Add a new entry:

```jsx
<SidebarLink to="/bookmarks" label="Bookmarks" />
```

## Step 4: Add an API method (if needed)

If your page needs a new API endpoint, add the Go handler first (see [Adding a Resolution Provider](/docs/local-dev/tutorials/tutorial-resolution-provider) for the backend pattern), then add the frontend wrapper in `frontend/src/services/api.js`:

```javascript
async listBookmarks(slug, params) {
  return this.request(`/repositories/${slug}/bookmarks`, { params });
}
```

## Step 5: Add RBAC (optional)

If the page requires a specific permission, gate it with the RBAC store:

```jsx
import { useRBAC } from "../../store/rbac";

const rbac = useRBAC();
const canRead = createMemo(() => rbac.hasPermission("bookmark", "read"));

return (
  <Layout>
    <Show
      when={canRead()}
      fallback={<p>You don't have permission to view bookmarks.</p>}
    >
      {/* page content */}
    </Show>
  </Layout>
);
```

## Tips

- Keep `index.jsx` under 150 lines — split sub-components into sibling files.
- Use `createResource` for async data fetching; it handles loading/error states.
- The `useRepository()` store gives you the current repo's `slug` and `id`.
- Use `Layout` from `components/Layout` for consistent page structure.
- Test the page size policy before pushing: `just check-frontend`.

## Summary

| File | Change |
|------|--------|
| `frontend/src/pages/Bookmarks/index.jsx` | New page component |
| `frontend/src/App.jsx` | Import + add `<Route>` |
| `frontend/src/components/Layout/Sidebar.jsx` | Add sidebar link (optional) |
| `frontend/src/services/api.js` | Add API method (if new endpoint needed) |
