// normalizeCitations rewrites every fact/concept-citation variant the
// LLM emits into numbered reference links: the first unique fact_id
// referenced becomes [1], the second [2], etc., so the rendered prose
// reads like an academic paper ("...as shown [1, 3]") instead of a
// dump of UUIDs. Each number is a markdown link to the fact detail
// route. Concept citations are routed to the concept detail route
// (/concepts/{id}) and tracked in a separate reference map so a fact
// and a concept never share a number.
//
// Extracted from pages/ConceptDetail/SummaryPanel.jsx so the
// DefinitionPanel (and any future consumer of fact-cited markdown)
// reuses the same normalization without duplication.
//
// Citation kinds. OKT stores facts and concepts in two separate UUID
// tables that share the v4 UUID space, so a bare UUID is ambiguous to a
// renderer. The canonical citation form carries a kind prefix:
//
//   [text](<fact:uuid>)      fact citation -> /facts/{id}
//   [name](<concept:uuid>)   concept citation -> /concepts/{id}
//   ![alt](<fact:uuid>)      image fact -> handled by normalizeImageCitations
//
// For backward compatibility with summaries produced before the prefix
// was introduced, the bare forms are still accepted and treated as
// FACT citations (the summarizer only ever emits fact citations, so
// the ambiguity is harmless in practice for legacy content):
//
//   1. [text](<uuid>)            — already markdown; keep the text,
//                                   rewrite the href, AND register the
//                                   uuid in the reference map so later
//                                   bare-bracket citations reuse the
//                                   same number.
//   2. ([ <uuid>]) / ([<uuid>])   — angle-bracketed uuid (with optional
//                                   leading space) inside parens, with
//                                   optional surrounding text.
//   3. [<uuid1>, <uuid2>, ...]    — comma-separated angle-bracketed
//                                   uuids inside one bracket pair.
//   4. [ <uuid>] / [<uuid>]       — a single angle-bracketed uuid
//                                   (optional leading space).
//   5. [uuid1, uuid2, ...]        — comma-separated bare uuids.
//   6. [uuid]                     — a single bare uuid.
//
// The prefixed forms are matched first, then the legacy bare forms.
//
// A comma group becomes one markdown link per uuid, comma-joined
// (e.g. "[1](...), [2](...)") so each fact is independently
// clickable — markdown links only accept a single URL, so the
// previous "[N facts](url1, url2)" output was invalid and rendered
// as broken links.
//
// The function is pure string -> string and runs before micromark,
// so the rendered HTML stays safe (micromark never passes raw HTML
// through regardless of the input). The reference map is scoped to
// one call (one summary slice / one definition), so numbers restart
// at [1] per call.
//
// Args:
//   - md:   the raw markdown the LLM produced.
//   - slug: the repo slug, used to build the fact/concept-detail href
//           (/{slug}/facts/{fact_id} or /{slug}/concepts/{concept_id}).

const UUID = "[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}";

// kindHref(slug, kind, id) returns the frontend route for a citation.
// "concept" routes to /{slug}/concepts/{id}; everything else (including
// the legacy bare-uuid form, which carries no kind) routes to
// /{slug}/facts/{id}.
function kindHref(slug, kind, id) {
  if (kind === "concept") return `/${slug}/concepts/${id}`;
  return `/${slug}/facts/${id}`;
}

export function normalizeCitations(md, slug) {
  if (!md) return "";
  const factRefNum = new Map();
  const conceptRefNum = new Map();
  let factCounter = 1;
  let conceptCounter = 1;
  const numFor = (kind, id) => {
    const map = kind === "concept" ? conceptRefNum : factRefNum;
    const counter = kind === "concept" ? () => conceptCounter++ : () => factCounter++;
    let n = map.get(id);
    if (n === undefined) {
      n = counter();
      map.set(id, n);
    }
    return n;
  };
  const link = (kind, id) => {
    const n = numFor(kind, id);
    return `[${n}](${kindHref(slug, kind, id)})`;
  };
  const linkList = (kind, ids) => ids.map((id) => link(kind, id)).join(", ");

  let out = md;

  // 0a. Canonical [text](<fact:uuid>) / [text](<concept:uuid>) — keep
  //     the text, rewrite the href, and register the uuid in the
  //     reference map. Match the prefixed form before the legacy
  //     bare-uuid form so a prefixed id is never mistaken for text.
  const prefixedLinkRe = new RegExp(
    `\\[([^\\]]*?)\\]\\(<\\s*(fact|concept)\\s*:\\s*(${UUID})\\s*>\\)`,
    "g"
  );
  out = out.replace(prefixedLinkRe, (_m, text, kind, id) => {
    numFor(kind, id);
    const label = text && text.trim() ? text.trim() : String(numFor(kind, id));
    return `[${label}](${kindHref(slug, kind, id)})`;
  });

  // 0b. Bare-bracket prefixed singletons: [<fact:uuid>] /
  //     [<concept:uuid>] and comma groups [<fact:u1>, <fact:u2>, ...]
  //     / [<concept:u1>, <concept:u2>, ...]. Comma groups must share
  //     one kind (the renderer can't mix fact and concept numbers in
  //     a single bracket pair cleanly); if the model ever mixes kinds
  //     we fall back to matching each id by its own prefixed singleton.
  const prefixedAngleGroupRe = new RegExp(
    `\\[\\s*(?:<\\s*(fact|concept)\\s*:\\s*${UUID}\\s*>\\s*,\\s*)+<\\s*(fact|concept)\\s*:\\s*${UUID}\\s*>\\s*\\]`,
    "g"
  );
  out = out.replace(prefixedAngleGroupRe, (m) => {
    // Extract the first kind to label the group; in practice the
    // model emits one kind per group. Parse (kind, id) pairs.
    const pairs = [...m.matchAll(/<(fact|concept)\s*:\s*([0-9a-fA-F-]{36})>/g)];
    if (pairs.length === 0) return m;
    const kind = pairs[0][1];
    const ids = pairs.map((p) => p[2]);
    return linkList(kind, ids);
  });
  const prefixedAngleSingleRe = new RegExp(
    `\\[\\s*<(fact|concept)\\s*:\\s*(${UUID})>\\s*\\]`,
    "g"
  );
  out = out.replace(prefixedAngleSingleRe, (_m, kind, id) => link(kind, id));

  // 1. Canonical legacy [text](<uuid>) — keep the text, rewrite the
  //    href, and register the uuid in the fact reference map (the
  //    summarizer only emits fact citations, so a bare legacy uuid is
  //    unambiguously a fact).
  const linkRe = new RegExp(
    `\\[([^\\]]*?)\\]\\((?:<)?(${UUID})(?:>)?\\)`,
    "g"
  );
  out = out.replace(linkRe, (_m, text, id) => {
    numFor("fact", id);
    const label = text && text.trim() ? text.trim() : String(numFor("fact", id));
    return `[${label}](${kindHref(slug, "fact", id)})`;
  });

  // 2. ([ <uuid>]) / ([<uuid>]) — angle-bracketed uuid inside parens.
  const parenRe = new RegExp(
    `(?<!\\])\\(([^()]*?)\\(?(?:<)?\\s*(${UUID})\\s*(?:>)?\\)?\\)`,
    "g"
  );
  out = out.replace(parenRe, (_m, pre, id) => {
    const label = pre && pre.trim() ? pre.trim() : String(numFor("fact", id));
    return `([${label}](${kindHref(slug, "fact", id)}))`;
  });

  // 3. [ <uuid1>, <uuid2>, ...] — comma-separated angle-bracketed
  //    uuids inside one bracket pair.
  const angleGroupRe = new RegExp(
    `\\[\\s*(?:<\\s*${UUID}\\s*>\\s*,\\s*)+<\\s*${UUID}\\s*>\\s*\\]`,
    "g"
  );
  out = out.replace(angleGroupRe, (m) => {
    const ids = [...m.matchAll(new RegExp(UUID, "g"))].map((x) => x[0]);
    return linkList("fact", ids);
  });

  // 4. [ <uuid>] / [<uuid>] — single angle-bracketed uuid.
  const angleSingleRe = new RegExp(`\\[\\s*<(${UUID})>\\s*\\]`, "g");
  out = out.replace(angleSingleRe, (_m, id) => link("fact", id));

  // 5. [uuid1, uuid2, ...] — comma-separated bare uuids.
  const bareGroupRe = new RegExp(
    `\\[\\s*(?:${UUID}\\s*,\\s*)+${UUID}\\s*\\]`,
    "g"
  );
  out = out.replace(bareGroupRe, (m) => {
    const ids = [...m.matchAll(new RegExp(UUID, "g"))].map((x) => x[0]);
    return linkList("fact", ids);
  });

  // 6. [uuid] — single bare uuid.
  const bareSingleRe = new RegExp(`\\[\\s*(${UUID})\\s*\\]`, "g");
  out = out.replace(bareSingleRe, (_m, id) => link("fact", id));

  return out;
}

// normalizeImageCitations rewrites ![alt](<fact:fact_id>) markdown image
// citations into ![alt](renderableUrl) using the supplied
// fact_id -> url map. The synthesis (definition) emits image
// citations in the ![alt](<fact:fact_id>) shape; micromark would render
// `![alt](<fact:fact_id>)` as a broken image because `<fact:fact_id>`
// is not a URL. The DefinitionPanel builds the map by resolving each
// embedded image fact's image_url (storage URLs become blob URLs via
// api.getSourceImage; external http(s) URLs pass through) and passes
// it here so the final markdown carries real, renderable URLs.
//
// The regex also accepts the legacy bare-<fact_id> form produced by
// older summaries before the "fact:" kind prefix was introduced.
//
// Citations whose fact_id is not in the map (e.g. a stale id the
// synthesis emitted but the eager-load didn't return) are replaced
// with the alt text in italics so the reader sees a placeholder
// rather than a broken image.
export function normalizeImageCitations(md, imageMap) {
  if (!md) return "";
  const imageUuidRe = new RegExp(
    `!\\[([^\\]]*)\\]\\(<\\s*(?:fact\\s*:\\s*)?(${UUID})\\s*>\\)`,
    "g"
  );
  return md.replace(imageUuidRe, (full, alt, id) => {
    const url = imageMap.get(id);
    if (!url) {
      // Drop the broken image; keep the alt text as italic placeholder.
      return alt ? `*${alt}*` : "";
    }
    return `![${alt}](${url})`;
  });
}