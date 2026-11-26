import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { fileURLToPath, URL } from "node:url";

// Backend dev target: the Fiber gateway (All-in-One) listens on :3100.
const API_TARGET = process.env.VITE_API_TARGET ?? "http://localhost:3100";

// Matches modules that live inside the given npm packages (pnpm-safe: anchors on the innermost node_modules).
const packages = (...names: string[]) =>
  new RegExp(`node_modules[\\\\/](?:${names.map((name) => name.replace("/", "[\\\\/]")).join("|")})[\\\\/]`);

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("./src", import.meta.url)),
      "@lazy-observe/browser": fileURLToPath(
        new URL("../sdk/browser/packages/browser/src/index.ts", import.meta.url),
      ),
      "@lazy-observe/react": fileURLToPath(
        new URL("../sdk/browser/packages/react/src/index.ts", import.meta.url),
      ),
      react: fileURLToPath(new URL("./node_modules/react", import.meta.url)),
      "react-dom": fileURLToPath(new URL("./node_modules/react-dom", import.meta.url)),
      rrweb: fileURLToPath(new URL("./node_modules/rrweb", import.meta.url)),
      "web-vitals": fileURLToPath(new URL("./node_modules/web-vitals", import.meta.url)),
    },
  },
  build: {
    rolldownOptions: {
      output: {
        // Vite 8 bundles with Rolldown, which has no object-form `manualChunks`. Each group below keeps the
        // semantics of the old Rollup entry: `test` selects the listed packages only (the trailing separator
        // stops `react` from also matching `react-i18next` or `@radix-ui/react-*`), and
        // `includeDependenciesRecursively` (default true) pulls in their dependencies. Groups are tried in
        // order, like the old object keys.
        codeSplitting: {
          groups: [
            // Keep the canvas time-series renderer out of the application shell.
            { name: "uplot", test: packages("uplot") },
            // rrweb (Session Replay engine) is only needed on the replay page; keep it out of the shell chunk.
            { name: "replay-engine", test: packages("rrweb") },
            // Keep CodeMirror and the Apache-2.0 Prometheus parser used by the MetricsQL editor out of the
            // application shell chunk.
            {
              name: "promql",
              test: packages(
                "@codemirror/autocomplete",
                "@codemirror/commands",
                "@codemirror/language",
                "@codemirror/state",
                "@codemirror/view",
                "@lezer/highlight",
                "@prometheus-io/lezer-promql",
              ),
            },
            {
              name: "vendor",
              test: packages("react", "react-dom", "@tanstack/react-router", "@tanstack/react-query"),
            },
            // Rolldown puts modules shared by the entry and several lazy routes into many small common chunks
            // (16 files on the first load without this group; Rollup folded them into the entry). Merge the
            // remaining node_modules code the entry loads statically into one chunk so the shell stays at a
            // handful of requests. It must stay last: earlier groups win for the packages they list.
            { name: "shell-vendor", test: /node_modules/, tags: ["$initial"] },
          ],
        },
      },
    },
  },
  // 单测：与迁移前的 `node --test` 用同一批 *.test.ts，断言仍是 node:assert/strict。
  test: {
    include: ["src/**/*.test.ts"],
    environment: "node",
  },
  server: {
    host: "0.0.0.0",
    port: 5173,
    strictPort: true,
    fs: {
      allow: [fileURLToPath(new URL("..", import.meta.url))],
    },
    proxy: {
      // Dev proxy: browser calls /api/* -> Fiber gateway. Keeps same-origin in prod
      // (frontend is go:embed'd behind the same gateway), so no code path changes.
      "/api": {
        target: API_TARGET,
        changeOrigin: true,
      },
      "/v1": {
        target: API_TARGET,
        changeOrigin: true,
      },
    },
  },
});
