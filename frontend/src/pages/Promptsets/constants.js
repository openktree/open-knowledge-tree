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