import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { vite as lazyObserve } from "@lazy-observe/bundler-plugin";

const uploadDsn = process.env.LO_E2E_DSN;
const release = process.env.LO_E2E_RELEASE ?? "react-example";
const journeyTarget = process.env.LO_E2E_JOURNEY_TARGET;
const journeyProxy = journeyTarget ? {
  "/journey-api": {
    target: journeyTarget,
    changeOrigin: true,
    rewrite: (path: string) => path.replace(/^\/journey-api/, ""),
  },
} : undefined;

export default defineConfig({
  build: { sourcemap: true },
  plugins: [
    react(),
    lazyObserve({
      dsn: uploadDsn ?? "http://127.0.0.1:3100/p/default@disabled",
      release,
      upload: Boolean(uploadDsn),
      stripSourceMaps: process.env.LO_E2E_STRIP_SOURCEMAPS === "1",
    }),
  ],
  server: journeyProxy ? { proxy: journeyProxy } : undefined,
  preview: journeyProxy ? { proxy: journeyProxy } : undefined,
});
