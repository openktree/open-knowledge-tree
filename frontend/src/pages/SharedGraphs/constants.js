// Tag presets for the Shared Graphs filter dropdown. These are
// suggestions, not an exhaustive list — the registry accepts any tag
// string on push. Add entries here as conventions emerge.
export const TAG_PRESETS = [
  "scientific",
  "general",
  "agriculture",
  "medicine",
  "policy",
  "education",
  "climate",
];

// Import mode labels for the import dialog.
export const IMPORT_MODE_NEW = "new";
export const IMPORT_MODE_EXISTING = "existing";

export const MODE_LABELS = {
  [IMPORT_MODE_NEW]: "Create a new repository from this graph",
  [IMPORT_MODE_EXISTING]: "Import into the current repository",
};
