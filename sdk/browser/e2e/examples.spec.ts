import { expect, test, type APIRequestContext, type Page } from "@playwright/test";

const platformURL = requiredEnv("LO_E2E_PLATFORM_URL");
const vanillaURL = requiredEnv("LO_E2E_VANILLA_URL");
const reactURL = requiredEnv("LO_E2E_REACT_URL");
const token = requiredEnv("LO_E2E_TOKEN");
const password = requiredEnv("LO_E2E_ADMIN_PASSWORD");
const dsn = `${platformURL}/p/default@${token}`;

interface SignalResponse {
  path: string;
  status: number;
  allowOrigin: string | undefined;
  payload: string | null;
}

function requiredEnv(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required`);
  return value;
}

function observeSignals(page: Page): SignalResponse[] {
  const signals: SignalResponse[] = [];
  page.on("response", (response) => {
    const request = response.request();
    if (request.method() !== "POST" || !response.url().startsWith(platformURL)) return;
    signals.push({
      path: new URL(response.url()).pathname,
      status: response.status(),
      allowOrigin: response.headers()["access-control-allow-origin"],
      payload: request.postData(),
    });
  });
  return signals;
}

async function waitForSignal(
  signals: SignalResponse[],
  path: string,
): Promise<SignalResponse> {
  let signal: SignalResponse | undefined;
  await expect.poll(() => {
    signal = signals.find((candidate) => candidate.path === path);
    return signal?.status;
  }).toBeGreaterThanOrEqual(200);
  expect(signal).toBeDefined();
  expect(signal!.status).toBeLessThan(300);
  expect(signal!.allowOrigin).toBe("*");
  return signal!;
}

// Project API tokens are ingest-only (ADR 0010): the token only builds the DSN,
// and every read rides on this session cookie plus X-LO-Project.
async function login(request: APIRequestContext): Promise<void> {
  const response = await request.post(`${platformURL}/api/auth/login`, {
    data: { username: "admin", password },
  });
  expect(response.ok()).toBeTruthy();
}

test.describe.configure({ mode: "serial" });

test("vanilla example sends errors, Web Vitals, Replay, and trace context", async ({
  page,
  request,
}) => {
  const signals = observeSignals(page);
  await page.goto(`${vanillaURL}/?dsn=${encodeURIComponent(dsn)}`);
  await expect(page.getByRole("heading", { name: "lazy-observe vanilla demo" })).toBeVisible();
  const sessionText = await page.locator("#output").innerText();
  const sessionID = sessionText.replace("Session: ", "");
  expect(sessionID).toMatch(/^[0-9a-f-]{36}$/);

  const tracedRequest = page.waitForRequest((candidate) => (
    candidate.resourceType() === "fetch" &&
    new URL(candidate.url()).pathname === "/" &&
    candidate.headers().traceparent !== undefined
  ));
  await page.getByRole("button", { name: "Traced request" }).click();
  await expect(page.locator("#output")).toHaveText("Request: 200");
  expect((await tracedRequest).headers().traceparent).toMatch(
    /^00-[0-9a-f]{32}-[0-9a-f]{16}-01$/,
  );

  await page.getByRole("button", { name: "Capture error" }).click();
  await expect(page.locator("#output")).toHaveText("Error captured");
  const errorSignal = await waitForSignal(signals, "/api/events/errors");
  expect(errorSignal.payload).toContain("vanilla demo error");
  await waitForSignal(signals, "/api/replay/segments");
  const vitalSignal = await waitForSignal(signals, "/api/events/vitals");
  expect(vitalSignal.payload).toMatch(/"(TTFB|FCP|LCP|CLS|INP)"/);

  await login(request);
  await expect.poll(async () => {
    const response = await request.get(`${platformURL}/api/issues`, {
      headers: { "X-LO-Project": "default" },
    });
    return response.ok() ? JSON.stringify(await response.json()) : "";
  }).toContain("vanilla demo error");

  await expect.poll(async () => {
    const response = await request.get(`${platformURL}/api/replay/sessions?has_error=true`, {
      headers: { "X-LO-Project": "default" },
    });
    return response.ok() ? JSON.stringify(await response.json()) : "";
  }).toContain(sessionID);

  const selector = `{__name__=~"web_vitals_(lcp|inp|cls|ttfb|fcp)",page="/"}`;
  await expect.poll(async () => {
    const response = await request.get(`${platformURL}/api/v1/query`, {
      headers: { "X-LO-Project": "default" },
      params: { query: selector },
    });
    if (!response.ok()) return 0;
    const body = await response.json() as { data?: { result?: unknown[] } };
    return body.data?.result?.length ?? 0;
  }).toBeGreaterThan(0);
});

test("React example captures a render failure through ErrorBoundary", async ({ page }) => {
  const signals = observeSignals(page);
  await page.goto(`${reactURL}/?dsn=${encodeURIComponent(dsn)}`);
  await expect(page.getByRole("heading", { name: "lazy-observe React demo" })).toBeVisible();
  await expect(page.getByLabel("Session ID")).toHaveText(/^[0-9a-f-]{36}$/);

  await page.getByRole("button", { name: "Crash render" }).click();
  await expect(page.getByText("Captured by ErrorBoundary.")).toBeVisible();
  const errorSignal = await waitForSignal(signals, "/api/events/errors");
  expect(errorSignal.payload).toContain("React demo render error");
});
