import { defineConfig } from "vite";
import solid from "vite-plugin-solid";

export default defineConfig({
  plugins: [solid()],
  server: {
    proxy: {
      "/api": process.env.VITE_API_TARGET || "http://localhost:8080",
    },
  },
});
