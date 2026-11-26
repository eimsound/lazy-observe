#!/usr/bin/env node
import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { createRequire } from "node:module";
import path from "node:path";
import { fileURLToPath } from "node:url";

const INGEST_PATHS = new Set([
  "/v1/traces",
  "/api/events/vitals",
  "/api/events/errors",
  "/api/events/diagnostics",
  "/api/replay/segments",
]);

function requiredEnv(name) {
  const value = process.env[name]?.trim();
  if (!value) {
    throw new Error(`${name} must be set`);
  }
  return value;
}

function appendNoProxy(hosts) {
  for (const key of ["NO_PROXY", "no_proxy"]) {
    const existing = (process.env[key] ?? "")
      .split(",")
      .map((item) => item.trim())
      .filter(Boolean);
    process.env[key] = Array.from(new Set([...existing, ...hosts])).join(",");
  }
}

function loadChromium() {
  const scriptDir = path.dirname(fileURLToPath(import.meta.url));
  const repoRoot = path.resolve(scriptDir, "..");
  const pkgBases = [
    path.join(repoRoot, "sdk", "browser", "package.json"),
    path.join(repoRoot, "web", "package.json"),
  ];
  for (const basePkg of pkgBases) {
    try {
      const req = createRequire(basePkg);
      try {
        return req("playwright-core").chromium;
      } catch {
        // Under pnpm strict node_modules, resolve playwright-core through @playwright/test -> playwright.
      }
      try {
        const pwTestPkg = req.resolve("@playwright/test/package.json");
        const reqFromTest = createRequire(pwTestPkg);
        const pwPkg = reqFromTest.resolve("playwright/package.json");
        const reqFromPw = createRequire(pwPkg);
        return reqFromPw("playwright-core").chromium;
      } catch {
        // Fall back to direct @playwright/test import if package.json subpath is not exported.
      }
      const pwTest = req("@playwright/test");
      if (pwTest?.chromium) {
        return pwTest.chromium;
      }
    } catch {
      // Try next candidate workspace.
    }
  }
  throw new Error("Could not resolve playwright-core from sdk/browser or web");
}

function isBrowserIngestURL(rawURL, baseURL) {
  try {
    const target = new URL(rawURL);
    const base = new URL(baseURL);
    if (target.origin !== base.origin) {
      return false;
    }
    return target.pathname.startsWith("/p/") || INGEST_PATHS.has(target.pathname);
  } catch {
    return false;
  }
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function pollUntil(label, timeoutMs, intervalMs, probe) {
  const deadline = Date.now() + timeoutMs;
  let lastDetail = "";
  while (Date.now() < deadline) {
    const outcome = await probe();
    if (outcome.ok) {
      return outcome.value;
    }
    if (outcome.detail !== undefined) {
      lastDetail = outcome.detail;
    }
    await sleep(intervalMs);
  }
  throw new Error(`Timed out after ${timeoutMs}ms waiting for ${label}${lastDetail ? `: ${lastDetail}` : ""}`);
}

async function loginApi(baseURL, password) {
  const response = await fetch(`${baseURL}/api/auth/login`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ username: "admin", password }),
  });
  assert.equal(response.ok, true, `admin API login failed with HTTP ${response.status}`);
  const rawCookies = typeof response.headers.getSetCookie === "function"
    ? response.headers.getSetCookie()
    : [];
  const cookieHeader = rawCookies
    .map((entry) => entry.split(";", 1)[0]?.trim() ?? "")
    .filter(Boolean)
    .join("; ");
  assert.ok(cookieHeader.includes("lss_"), `expected session cookie on login, got: ${cookieHeader}`);
  return cookieHeader;
}

async function apiGetJson(baseURL, cookie, projectSlug, pathAndQuery) {
  const response = await fetch(`${baseURL}${pathAndQuery}`, {
    headers: {
      Cookie: cookie,
      "X-LO-Project": projectSlug,
    },
  });
  const text = await response.text();
  if (!response.ok) {
    return { ok: false, status: response.status, json: null, text };
  }
  try {
    return { ok: true, status: response.status, json: JSON.parse(text), text };
  } catch {
    return { ok: false, status: response.status, json: null, text };
  }
}

function attachListeners(page, baseURL, state) {
  page.on("pageerror", () => {
    // Expected unhandled errors are triggered deliberately to verify capture.
  });

  page.on("request", (request) => {
    const url = request.url();
    if (request.method() === "POST" && isBrowserIngestURL(url, baseURL)) {
      const entry = {
        phase: state.phase,
        pathname: new URL(url).pathname,
        url,
        postData: request.postData() ?? "",
      };
      state.ingestRequests.push(entry);
    }
  });

  page.on("response", async (response) => {
    const request = response.request();
    const url = response.url();
    let pathname = "";
    try {
      pathname = new URL(url).pathname;
    } catch {
      return;
    }

    if (request.method() === "GET" && pathname === "/api/system/self-monitoring") {
      let bodyText = "";
      let bodyJson = null;
      try {
        bodyText = await response.text();
        bodyJson = JSON.parse(bodyText);
      } catch {
        // Ignore parse failure for non-JSON responses.
      }
      state.bootstrapResponses.push({
        status: response.status(),
        headers: response.headers(),
        bodyText,
        bodyJson,
      });
      return;
    }

    if (request.method() === "POST" && isBrowserIngestURL(url, baseURL)) {
      state.ingestResponses.push({
        phase: state.phase,
        pathname,
        status: response.status(),
        postData: request.postData() ?? "",
      });
    }
  });
}

async function signInViaUI(page, baseURL, adminPassword) {
  await page.goto(`${baseURL}/login`, { waitUntil: "domcontentloaded" });
  const usernameInput = page.locator("#username");
  await usernameInput.waitFor({ state: "visible", timeout: 15_000 });
  const loginHtml = await page.content();

  await usernameInput.fill("admin");
  await page.locator("#password").fill(adminPassword);
  await page.locator('button[type="submit"]').click();

  const userMenu = page.getByRole("button", { name: /User menu|用户菜单/ });
  await userMenu.waitFor({ state: "visible", timeout: 20_000 });
  return { loginHtml, userMenu };
}

async function verifyEnabledMode(browser, baseURL, adminPassword) {
  const runId = randomBytes(4).toString("hex");
  const errorMarker = `web-self-mon-e2e-${runId}`;
  const postLogoutError = `web-self-mon-after-logout-${runId}`;

  const context = await browser.newContext();
  const page = await context.newPage();
  const state = {
    phase: "unauth",
    bootstrapResponses: [],
    ingestRequests: [],
    ingestResponses: [],
  };
  attachListeners(page, baseURL, state);

  try {
    // 1. Unauthenticated /login: 401 on /api/system/self-monitoring and zero lo_ token exposure.
    await page.goto(`${baseURL}/login`, { waitUntil: "domcontentloaded" });
    await page.locator("#username").waitFor({ state: "visible", timeout: 15_000 });
    const loginHtml = await page.content();
    assert.equal(
      /\blo_[A-Za-z0-9_-]{16,}\b/.test(loginHtml),
      false,
      "unauthenticated login HTML must not contain an lo_ token",
    );
    const unauthStatus = await page.evaluate(async () => {
      const resp = await fetch("/api/system/self-monitoring", { credentials: "same-origin" });
      return resp.status;
    });
    assert.equal(unauthStatus, 401, `expected unauthenticated /api/system/self-monitoring 401, got ${unauthStatus}`);
    assert.equal(state.ingestRequests.length, 0, "must not emit browser ingest requests before login");

    // 2. Sign in via UI -> bootstrap activates @lazy-observe/browser.
    state.phase = "authed";
    await page.locator("#username").fill("admin");
    await page.locator("#password").fill(adminPassword);
    await page.locator('button[type="submit"]').click();
    const userMenu = page.getByRole("button", { name: /User menu|用户菜单/ });
    await userMenu.waitFor({ state: "visible", timeout: 20_000 });

    const authedBootstrap = await pollUntil("authenticated /api/system/self-monitoring 200", 20_000, 200, async () => {
      const hit = state.bootstrapResponses.find((entry) => entry.status === 200);
      return hit ? { ok: true, value: hit } : { ok: false, detail: JSON.stringify(state.bootstrapResponses) };
    });

    const cacheControl = (authedBootstrap.headers["cache-control"] ?? "").toLowerCase();
    assert.ok(cacheControl.includes("no-store"), `expected Cache-Control: no-store, got ${cacheControl}`);
    assert.equal(authedBootstrap.bodyJson?.enabled, true);
    assert.equal(authedBootstrap.bodyJson?.project_slug, "lazy-observe");
    assert.equal(authedBootstrap.bodyJson?.service_name, "lazy-observe-web");
    assert.ok(
      typeof authedBootstrap.bodyJson?.token === "string" &&
      authedBootstrap.bodyJson.token.startsWith("lo_") &&
      authedBootstrap.bodyJson.token.length > 10,
      `invalid self-monitoring token payload: ${authedBootstrap.bodyText}`,
    );
    assert.ok(!authedBootstrap.bodyJson?.replay_enabled, "expected replay_enabled to default to false");
    assert.equal(loginHtml.includes(authedBootstrap.bodyJson.token), false);

    await pollUntil("SDK sessionStorage initialization", 15_000, 200, async () => {
      const sessionId = await page.evaluate(() => window.sessionStorage.getItem("lazy-observe:session-id"));
      return sessionId ? { ok: true, value: sessionId } : { ok: false, detail: "session-id missing" };
    });

    // 3. Navigate UI routes (finalizes LCP + sets page), run same-origin fetch, and trigger an unhandled error.
    await page.locator('a[href="/logs"]').first().click();
    await page.waitForURL(/\/logs/, { timeout: 15_000 });
    const projectsStatus = await page.evaluate(async () => {
      const resp = await fetch("/api/projects", { credentials: "same-origin" });
      return resp.status;
    });
    assert.equal(projectsStatus, 200, `expected GET /api/projects 200, got ${projectsStatus}`);

    await page.evaluate((msg) => {
      setTimeout(() => {
        throw new Error(msg);
      }, 0);
    }, errorMarker);

    await pollUntil("browser trace/vital/error ingest requests", 30_000, 300, async () => {
      const authedResponses = state.ingestResponses.filter(
        (entry) => entry.phase === "authed" && entry.status >= 200 && entry.status < 300,
      );
      const hasTrace = authedResponses.some(
        (entry) => entry.pathname === "/v1/traces" && entry.postData.includes("lazy-observe-web"),
      );
      const hasVital = authedResponses.some(
        (entry) => entry.pathname === "/api/events/vitals" && /"(TTFB|FCP|LCP|CLS|INP)"/.test(entry.postData),
      );
      const hasError = authedResponses.some(
        (entry) => entry.pathname === "/api/events/errors" && entry.postData.includes(errorMarker),
      );
      if (hasTrace && hasVital && hasError) {
        return { ok: true, value: true };
      }
      return {
        ok: false,
        detail: `hasTrace=${hasTrace} hasVital=${hasVital} hasError=${hasError} seen=${JSON.stringify(authedResponses.map((r) => [r.pathname, r.status]))}`,
      };
    });

    // 4. Read back Trace, Web Vital, and Issue in the built-in lazy-observe project (and verify project isolation).
    const cookie = await loginApi(baseURL, adminPassword);
    const vitalsQuery = encodeURIComponent('{__name__=~"web_vitals_(lcp|inp|cls|ttfb|fcp)"}');
    await pollUntil("real-backend readback in lazy-observe project", 45_000, 1_000, async () => {
      const [svcRes, searchRes, vitalRes, issueRes] = await Promise.all([
        apiGetJson(baseURL, cookie, "lazy-observe", "/api/traces/services"),
        apiGetJson(baseURL, cookie, "lazy-observe", "/api/traces/search?service=lazy-observe-web&limit=20"),
        apiGetJson(baseURL, cookie, "lazy-observe", `/api/v1/query?query=${vitalsQuery}`),
        apiGetJson(baseURL, cookie, "lazy-observe", `/api/issues?q=${encodeURIComponent(errorMarker)}`),
      ]);
      const hasTraceService = svcRes.ok && Array.isArray(svcRes.json?.data) && svcRes.json.data.includes("lazy-observe-web");
      const hasTraceData = searchRes.ok && Array.isArray(searchRes.json?.data) && searchRes.json.data.length > 0;
      const hasVitalMetric = vitalRes.ok && Array.isArray(vitalRes.json?.data?.result) && vitalRes.json.data.result.length > 0;
      const hasErrorIssue =
        issueRes.ok &&
        Array.isArray(issueRes.json?.issues) &&
        issueRes.json.issues.some((item) => typeof item?.title === "string" && item.title.includes(errorMarker));

      if (hasTraceService && hasTraceData && hasVitalMetric && hasErrorIssue) {
        return { ok: true, value: true };
      }
      return {
        ok: false,
        detail: `svc=${hasTraceService} trace=${hasTraceData} vital=${hasVitalMetric} issue=${hasErrorIssue}`,
      };
    });

    const defaultIssueRes = await apiGetJson(
      baseURL,
      cookie,
      "default",
      `/api/issues?q=${encodeURIComponent(errorMarker)}`,
    );
    assert.equal(defaultIssueRes.ok, true);
    assert.equal(
      (defaultIssueRes.json?.issues ?? []).some(
        (item) => typeof item?.title === "string" && item.title.includes(errorMarker),
      ),
      false,
      "self-monitoring issue must not leak into default project",
    );

    // 5. Sign out via UI topbar -> sessionStorage cleared and zero telemetry afterwards.
    await page.evaluate(() => {
      window.sessionStorage.setItem("lazy-observe:replay-seq:e2e-check", "1");
      window.sessionStorage.setItem("lo-e2e-keep", "keep");
    });

    await userMenu.click();
    await page.getByRole("menuitem", { name: /Sign out|退出登录/ }).click();
    await page.waitForURL(/\/login/, { timeout: 15_000 });
    await page.locator("#username").waitFor({ state: "visible", timeout: 15_000 });

    const remainingKeys = await page.evaluate(() => Object.keys(window.sessionStorage));
    assert.equal(remainingKeys.includes("lazy-observe:session-id"), false, "session-id must be cleared on sign-out");
    assert.equal(
      remainingKeys.some((key) => key.startsWith("lazy-observe:replay-seq:")),
      false,
      "replay-seq keys must be cleared on sign-out",
    );
    assert.equal(
      await page.evaluate(() => window.sessionStorage.getItem("lo-e2e-keep")),
      "keep",
      "unrelated sessionStorage keys must stay intact",
    );

    // Allow any pre-logout close() flush (e.g. POST /api/auth/logout span) to finish before the post-logout window.
    await sleep(1_000);
    state.phase = "post-logout";

    await page.evaluate(async (msg) => {
      await fetch("/healthz", { credentials: "same-origin" });
      setTimeout(() => {
        throw new Error(msg);
      }, 0);
    }, postLogoutError);

    await sleep(6_500);
    const postLogoutIngest = state.ingestRequests.filter((entry) => entry.phase === "post-logout");
    assert.equal(
      postLogoutIngest.length,
      0,
      `expected zero ingest requests after sign-out, saw: ${JSON.stringify(postLogoutIngest)}`,
    );

    const postLogoutIssueRes = await apiGetJson(
      baseURL,
      cookie,
      "lazy-observe",
      `/api/issues?q=${encodeURIComponent(postLogoutError)}`,
    );
    assert.equal(postLogoutIssueRes.ok, true);
    assert.equal(
      (postLogoutIssueRes.json?.issues ?? []).some(
        (item) => typeof item?.title === "string" && item.title.includes(postLogoutError),
      ),
      false,
      "post-logout error must not be ingested into lazy-observe project",
    );
  } finally {
    await context.close();
  }
}

async function verifyDisabledMode(browser, baseURL, adminPassword) {
  const runId = randomBytes(4).toString("hex");
  const disabledError = `web-self-mon-disabled-${runId}`;

  const context = await browser.newContext();
  const page = await context.newPage();
  const state = {
    phase: "disabled",
    bootstrapResponses: [],
    ingestRequests: [],
    ingestResponses: [],
  };
  attachListeners(page, baseURL, state);

  try {
    await signInViaUI(page, baseURL, adminPassword);

    const bootstrapResp = await pollUntil("disabled /api/system/self-monitoring 200", 20_000, 200, async () => {
      const hit = state.bootstrapResponses.find((entry) => entry.status === 200);
      return hit ? { ok: true, value: hit } : { ok: false, detail: JSON.stringify(state.bootstrapResponses) };
    });

    const cacheControl = (bootstrapResp.headers["cache-control"] ?? "").toLowerCase();
    assert.ok(cacheControl.includes("no-store"), `expected Cache-Control: no-store, got ${cacheControl}`);
    assert.deepEqual(bootstrapResp.bodyJson, { enabled: false });

    await sleep(1_000);
    assert.equal(
      await page.evaluate(() => window.sessionStorage.getItem("lazy-observe:session-id")),
      null,
      "SDK must not initialize sessionStorage when web=false",
    );

    await page.locator('a[href="/logs"]').first().click();
    await page.waitForURL(/\/logs/, { timeout: 15_000 });
    await page.evaluate(async (msg) => {
      await fetch("/api/projects", { credentials: "same-origin" });
      setTimeout(() => {
        throw new Error(msg);
      }, 0);
    }, disabledError);

    await sleep(6_500);
    assert.equal(
      state.ingestRequests.length,
      0,
      `expected zero ingest requests when self_monitoring.web=false, saw: ${JSON.stringify(state.ingestRequests)}`,
    );
  } finally {
    await context.close();
  }
}

async function main() {
  const mode = requiredEnv("LO_SELF_MON_MODE");
  const baseURL = requiredEnv("LO_SELF_MON_URL").replace(/\/+$/, "");
  const adminPassword = requiredEnv("LO_SELF_MON_ADMIN_PASSWORD");
  appendNoProxy(["127.0.0.1", "localhost", new URL(baseURL).hostname]);

  const chromium = loadChromium();
  const browser = await chromium.launch({ headless: true });
  try {
    if (mode === "enabled") {
      await verifyEnabledMode(browser, baseURL, adminPassword);
    } else if (mode === "disabled") {
      await verifyDisabledMode(browser, baseURL, adminPassword);
    } else {
      throw new Error(`Unsupported LO_SELF_MON_MODE: ${mode} (expected enabled or disabled)`);
    }
    console.log(`PASS: web self-monitoring E2E (${mode})`);
  } finally {
    await browser.close();
  }
}

await main();
