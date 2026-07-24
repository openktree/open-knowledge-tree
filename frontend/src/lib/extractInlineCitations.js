// extractInlineCitations: dedupe inline fact/concept citation UUIDs from cited markdown.
const UUID = "[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}";

const citationRe = new RegExp(`(?:\\b(fact|concept)\\s*:\\s*(${UUID})|<\\s*(${UUID})\\s*>)`, "g");

const fencedCodeRe = /```.*?```/gs;

export function extractInlineCitations(md) {
  if (!md) return { factIds: [], conceptIds: [] };
  const cleaned = md.replace(fencedCodeRe, "");
  const factSeen = new Set();
  const conceptSeen = new Set();
  const factIds = [];
  const conceptIds = [];
  let m;
  citationRe.lastIndex = 0;
  while ((m = citationRe.exec(cleaned)) !== null) {
    const prefix = m[1] || "";
    const uuid = m[2] || m[3];
    if (!uuid) continue;
    if (prefix.toLowerCase() === "concept") {
      if (!conceptSeen.has(uuid)) {
        conceptSeen.add(uuid);
        conceptIds.push(uuid);
      }
    } else if (!factSeen.has(uuid)) {
      factSeen.add(uuid);
      factIds.push(uuid);
    }
  }
  return { factIds, conceptIds };
}
