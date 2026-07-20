import { createRoot, createSignal } from "solid-js";

function createThemeStore() {
  const stored = localStorage.getItem("theme");
  const prefersDark = window.matchMedia("(prefers-color-scheme: dark)").matches;
  const initial = stored || (prefersDark ? "dark" : "light");

  const [theme, setTheme] = createSignal(initial);

  const toggle = () => {
    setTheme((t) => {
      const next = t === "dark" ? "light" : "dark";
      localStorage.setItem("theme", next);
      document.documentElement.classList.toggle("dark", next === "dark");
      return next;
    });
  };

  document.documentElement.classList.toggle("dark", initial === "dark");

  return { theme, toggle };
}

const store = createRoot(createThemeStore);

export function useTheme() {
  return store;
}
