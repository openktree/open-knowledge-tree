// Vitest configuration for the frontend JS unit tests. The lib
// modules (wrapSentences, citedCopy) need a DOM (DOMParser,
// TextNode, splitText), so we default to the jsdom environment.
// Tests live next to the modules they cover as *.test.mjs so vitest's
// default glob picks them up without extra config.
import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    environment: "jsdom",
    include: ["src/**/*.test.mjs"],
  },
});
