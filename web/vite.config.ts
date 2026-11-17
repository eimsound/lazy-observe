import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { fileURLToPath, URL } from "node:url";

// Backend dev target: the Fiber gateway (All-in-One) listens on :3100.
const API_TARGET = process.env.VITE_API_TARGET ?? "http://localhost:3100";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("./src", import.meta.url)),
    },
  },
  build: {
    rollupOptions: {
      output: {
        manualChunks: {
          // Keep the canvas time-series renderer out of the application shell.
          uplot: ["uplot"],
          // rrweb (Session Replay engine) is only needed on the replay page; keep
          // it out of the shell chunk.
          "replay-engine": ["rrweb"],
          // Keep CodeMirror and the Apache-2.0 Prometheus parser used by the
          // MetricsQL editor out of the application shell chunk.
          promql: [
            "@codemirror/autocomplete",
            "@codemirror/commands",
            "@codemirror/language",
            "@codemirror/state",
            "@codemirror/view",
            "@lezer/highlight",
            "@prometheus-io/lezer-promql",
          ],
          vendor: [
            "react",
            "react-dom",
            "@tanstack/react-router",
            "@tanstack/react-query",
          ],
        },
      },
    },
  },
  server: {
    host: "0.0.0.0",
    port: 5173,
    strictPort: true,
    proxy: {
      // Dev proxy: browser calls /api/* -> Fiber gateway. Keeps same-origin in prod
      // (frontend is go:embed'd behind the same gateway), so no code path changes.
      "/api": {
        target: API_TARGET,
        changeOrigin: true,
      },
    },
  },
});
