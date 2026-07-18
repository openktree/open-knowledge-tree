---
id: tutorial-repository-preset
sidebar_position: 6
title: Adding a Repository Preset
---

# Tutorial: Adding a Repository Preset

Repository presets are the "types" users see when creating a new repository — General, Scientific, Enterprise. Each preset bundles a curated set of search/resolution providers and a context allow-list. This tutorial shows how to add a new one — for example, a "Medical" preset.

## How presets work

Presets are defined in `config.default.yaml` under `repository_presets`. Each entry has:

- **`id`** — unique identifier (used as the selector)
- **`label`** — display name in the UI
- **`description`** — shown below the label
- **`providers`** — which search and resolution providers are enabled by default
- **`contexts`** — the curated context vocabulary (`["all"]` for everything, or a specific list)
- **`custom_contexts`** — non-standard labels to seed (optional)

The `default_repository_preset` field names which preset is pre-selected when the create form opens.

## Step 1: Add the preset

In `backend/configs/config.default.yaml`, add a new entry to `repository_presets`:

```yaml
repository_presets:
  # ... existing presets ...

  - id: medical
    label: Medical
    description: "OpenAlex + Unpaywall for open-access papers, medical and life-science contexts."
    providers:
      search: ["openalex"]
      resolution: ["fetch", "unpaywall", "tls"]
    contexts:
      - Biomolecule
      - drug
      - disease
      - Medicine
      - medical specialty
      - gene
      - protein
      - enzyme
      - organ
      - HumanGene
      - chemical compound
      - chemical substance
      - chemical element
      - research project
      - scientist
      - academic journal
      - article
      - book
      - species
```

That's it. The preset appears in the create-repository UI immediately.

## Step 2: Set it as the default (optional)

If you want "Medical" to be the pre-selected option:

```yaml
default_repository_preset: medical
```

## Step 3: Custom contexts (optional)

If the preset needs non-standard context labels (beyond the embedded 88 DBpedia labels), add them:

```yaml
  - id: medical
    label: Medical
    description: "..."
    providers:
      search: ["openalex"]
      resolution: ["fetch", "unpaywall", "tls"]
    contexts: ["all"]           # Start with everything
    custom_contexts:
      - Clinical Trial
      - Patient Outcome
      - Drug Interaction
    custom_context_descriptions:
      Clinical Trial: "A controlled study evaluating a medical intervention."
      Patient Outcome: "A measurable result of a medical treatment."
      Drug Interaction: "A reaction between two or more pharmaceuticals."
```

Custom contexts are seeded with `is_custom=TRUE` in the `repository_contexts` table, so they appear alongside the standard contexts.

## How the preset is applied

When a user creates a repository with `preset: "medical"`:

1. The API looks up the preset by `id` in the config.
2. It seeds the repository's provider settings from `providers.search` and `providers.resolution`.
3. It resolves `contexts` — `"all"` expands to the full embedded vocabulary; a specific list seeds only those contexts.
4. Custom contexts from `custom_contexts` are added with their descriptions.

The user can still toggle providers and contexts on the Settings page after creation — the preset is just the starting point.

## Summary

| File | Change |
|------|--------|
| `backend/configs/config.default.yaml` | Add entry to `repository_presets` list |

No code changes needed — presets are entirely config-driven.
