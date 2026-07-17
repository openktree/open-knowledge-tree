// Small shared helpers for the source-image gallery subcomponents.
// Kept in a single file so the gallery pieces can import the helpers
// without duplicating them.

import Badge from "../../components/Badge";

const ALT_BADGE_LIMIT = 40;

export function shortenURL(u) {
  if (!u) return "";
  try {
    const parsed = new URL(u);
    return parsed.pathname.length > 1 ? parsed.host + parsed.pathname : parsed.host;
  } catch {
    return u;
  }
}

export function AltBadge(props) {
  const text = () => (props.alt || "").trim();
  const hasAlt = () => text().length > 0;
  const display = () => {
    const t = text();
    return t.length > ALT_BADGE_LIMIT ? t.slice(0, ALT_BADGE_LIMIT - 1) + "…" : t;
  };
  return (
    <Badge variant={hasAlt() ? "green" : "gray"}>
      {hasAlt() ? `alt: ${display()}` : "no alt"}
    </Badge>
  );
}