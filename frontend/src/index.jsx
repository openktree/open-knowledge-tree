import { Router } from "@solidjs/router";
import { render } from "solid-js/web";
import App from "./App";
import "./index.css";

const stored = localStorage.getItem("theme");
const prefersDark = window.matchMedia("(prefers-color-scheme: dark)").matches;
if (stored === "dark" || (!stored && prefersDark)) {
  document.documentElement.classList.add("dark");
}

render(
  () => (
    <Router>
      <App />
    </Router>
  ),
  document.getElementById("root"),
);
