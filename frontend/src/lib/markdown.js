import { micromark } from "micromark";
import { gfm, gfmHtml } from "micromark-extension-gfm";

// Singleton micromark options. Micromark never passes raw HTML
// through by default (no `allowDangerousHtml`), so the rendered
// output is safe to mount via innerHTML — there is no path from
// a <script> tag in the source Markdown to a live node in the
// DOM. The GFM extension matches what the backend's
// html-to-markdown GitHub-flavored plugin produces, so the
// round-trip (backend ContentNode → Markdown → frontend HTML)
// preserves tables, strikethrough, autolinks, and task lists.
const options = {
  extensions: [gfm()],
  htmlExtensions: [gfmHtml()],
};

// renderMarkdown converts a Markdown string to an HTML string
// that is safe to mount via innerHTML. Returns "" for empty
// input so callers can render without an extra guard.
export function renderMarkdown(md) {
  if (!md) return "";
  return micromark(String(md), options);
}
