import { defineConfig, devices } from "@playwright/test";

const host = "127.0.0.1";
const port = 4173;
const baseURL = `http://${host}:${port}`;

// Playwright's webServer probe uses Node's proxy handling, which does not
// consistently recognize CIDR entries such as 127.0.0.0/8 in NO_PROXY.
process.env.NO_PROXY = [process.env.NO_PROXY, host].filter(Boolean).join(",");
process.env.no_proxy = [process.env.no_proxy, host].filter(Boolean).join(",");

export default defineConfig({
  testDir: "./e2e",
  outputDir: "./test-results",
  forbidOnly: Boolean(process.env.CI),
  retries: process.env.CI ? 1 : 0,
  workers: process.env.CI ? 1 : undefined,
  reporter: process.env.CI ? "line" : "list",
  use: {
    baseURL,
    locale: "zh-CN",
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"] },
    },
  ],
  webServer: {
    command: `pnpm dev --host ${host} --port ${port}`,
    url: `${baseURL}/login`,
    env: { VITE_MOCK: "1" },
    reuseExistingServer: !process.env.CI,
    timeout: 120_000,
  },
});
