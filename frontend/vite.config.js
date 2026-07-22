import { defineConfig } from "vite";
import solid from "vite-plugin-solid";

export default defineConfig({
  plugins: [solid()],
  server: {
    proxy: {
      "/api": {
        target: process.env.VITE_API_TARGET || "http://localhost:8080",
        // Graph export/download builds a whole-repo bundle in-process
        // and can take 20-60s for large repos; the default proxy
        // timeout hangs the socket before the backend responds.
        timeout: 300000,
      },
    },
  },
});
