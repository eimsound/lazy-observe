import { vite as lazyObserve } from "../../src/index";

export default {
  build: { sourcemap: true },
  plugins: [lazyObserve({ dsn: process.env.LO_DSN!, release: "vite-e2e" })]
};
