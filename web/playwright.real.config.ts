import { defineConfig, devices } from "@playwright/test";

const baseURL = process.env.LO_REAL_E2E_BASE_URL?.trim() || "http://127.0.0.1:8080";

function hostFromURL(raw: string): string {
  try {
    return new URL(raw).hostname;
  } catch {
    return "127.0.0.1";
  }
}

const targetHost = hostFromURL(baseURL);
const noProxyHosts = Array.from(new Set(["127.0.0.1", "localhost", targetHost]));

// Playwright's request/browser networking reads NO_PROXY / no_proxy and may not
// match CIDR entries such as 127.0.0.0/8 consistently across Node/Chromium.
process.env.NO_PROXY = [process.env.NO_PROXY, ...noProxyHosts].filter(Boolean).join(",");
process.env.no_proxy = [process.env.no_proxy, ...noProxyHosts].filter(Boolean).join(",");

export default defineConfig({
  testDir: "./e2e-real",
  outputDir: "./test-results/real",
  forbidOnly: Boolean(process.env.CI),
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 90_000,
  expect: {
    timeout: 15_000,
  },
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
});
