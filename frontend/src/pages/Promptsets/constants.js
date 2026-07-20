// Phase labels for the 8 promptset phases, in declaration order
// (matches the backend promptset.Promptset struct). Used by the
// Promptsets page form + the RepositorySettings promptset panel.
export const PHASE_LABELS = {
  fact_extraction: "Fact Extraction",
  image_fact_extraction: "Image Fact Extraction",
  concept_extraction: "Concept Extraction",
  refinement: "Refinement",
  synthesis: "Synthesis",
  image_picker: "Image Picker",
  summarization: "Summarization",
  posture: "Posture",
};

// PHASE_KEYS is the ordered list of phase field names, matching the
// backend's hashInput struct field order so the client-computed hash
// (used for display) matches the server's.
export const PHASE_KEYS = [
  "fact_extraction",
  "image_fact_extraction",
  "concept_extraction",
  "refinement",
  "synthesis",
  "image_picker",
  "summarization",
  "posture",
];

// REGISTRY_SHARED_KEYS is the subset of phases whose prompts are
// pushed to the registry and therefore feed the REGISTRY-compatibility
// hash. Changes to phases NOT in this list (synthesis, image_picker,
// summarization, posture) produce a new catalog hash but keep the
// same registry hash — i.e. a "compatible" promptset whose
// decompositions can be pulled by repos that only know the original.
// Mirrors promptset.RegistrySharedPhases in the backend.
export const REGISTRY_SHARED_KEYS = [
  "fact_extraction",
  "image_fact_extraction",
  "concept_extraction",
  "refinement",
];

// BUILTIN_SOURCE / CUSTOM_SOURCE mirror the backend promptset
// package constants so the UI can badge promptsets by origin.
export const BUILTIN_SOURCE = "builtin";
export const CUSTOM_SOURCE = "custom";

// emptyDraft returns a fresh blank draft for the create form.
export function emptyDraft() {
  const d = { name: "" };
  for (const k of PHASE_KEYS) d[k] = "";
  return d;
}

// draftFromPromptset builds a draft pre-populated from an existing
// custom promptset for the edit form.
export function draftFromPromptset(ps) {
  const d = { name: ps.name || "" };
  for (const k of PHASE_KEYS) d[k] = ps[k] || "";
  return d;
}

// computeRegistryHash computes the canonical SHA-256 hex digest over
// only the 4 registry-shared phases of a draft/promptset, matching
// the backend's promptset.RegistryHashPromptset. Used by the form
// to show a live "Registry compatibility" preview as the user
// edits the shared fields, and by the table to badge
// "≡ default"-compatible promptsets.
//
// Uses crypto.subtle (available in all modern browsers + secure
// contexts). Returns a Promise<string>.
export async function computeRegistryHash(draft) {
  const inObj = {};
  for (const k of REGISTRY_SHARED_KEYS) inObj[k] = draft?.[k] || "";
  const json = JSON.stringify(inObj);
  const enc = new TextEncoder().encode(json);
  const buf = await crypto.subtle.digest("SHA-256", enc);
  const bytes = new Uint8Array(buf);
  let hex = "";
  for (const b of bytes) hex += b.toString(16).padStart(2, "0");
  return hex;
}
