export const PHASES = [
  { id: "sources", label: "Sources", disabled: false },
  { id: "facts", label: "Facts", disabled: false },
  { id: "concepts", label: "Concepts", disabled: false },
];

export const ACTIVE_PHASE_IDS = PHASES.filter((p) => !p.disabled).map((p) => p.id);
