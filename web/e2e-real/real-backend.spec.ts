import { existsSync } from "node:fs";
import { expect, test } from "@playwright/test";
import {
  buildErrorEventPayload,
  buildOtlpTracePayload,
  encodeOtlpGaugeMetric,
  encodeOtlpLogRecord,
  ensureChineseLocale,
  loginViaUI,
  randomHex,
  readInitialAdminPassword,
  requiredEnv,
  signOutViaTopbar,
  switchProjectViaSidebar,
} from "./helpers";

const runId = randomHex(4);
const initialPasswordFile = requiredEnv("LO_REAL_E2E_ADMIN_PASSWORD_FILE");
const initialAdminPassword = readInitialAdminPassword();
const updatedAdminPassword = `RealE2E!${runId}Pass`;
const selfMonitoringEnabled =
  process.env.LO_REAL_E2E_SELF_MONITORING === "true" ||
  process.env.LO_REAL_E2E_SELF_MONITORING === "1";
const selfMonitoringWebDisabled =
  process.env.LO_REAL_E2E_SELF_MONITORING_WEB === "false" ||
  process.env.LO_REAL_E2E_SELF_MONITORING_WEB === "0";

const projectName = `Real E2E ${runId}`;
const projectSlug = `real-e2e-${runId}`;
const tokenName = `ingest-${runId}`;

const logMarker = `lo-real-e2e-log-${runId}`;
const logService = `real-e2e-log-svc-${runId}`;
const metricName = `lo_real_e2e_orders_total_${runId}`;
const metricService = `real-e2e-metrics-svc-${runId}`;
const traceId = randomHex(16);
const spanId = randomHex(8);
const traceService = `real-e2e-trace-svc-${runId}`;
const traceOperation = `GET /real-e2e/checkout/${runId}`;
const errorEventId = `evt-real-e2e-${runId}`;
const errorType = "RealE2ECheckoutError";
const errorValue = `checkout-failed-${runId}`;
const inhibitionComment = `Real E2E 抑制规则 ${runId}`;

let issuedProjectToken = "";

test.describe.configure({ mode: "serial" });

test("1. 未登录访问受保护页面重定向到 /login，匿名 API 请求被 401 拒绝", async ({ page, request }) => {
  await ensureChineseLocale(page);

  await page.goto("/settings/projects?source=real-e2e");
  await expect(page).toHaveURL(/\/login\?redirect=%2Fsettings%2Fprojects%3Fsource%3Dreal-e2e$/);
  await expect(page.getByRole("heading", { name: "登录" })).toBeVisible();
  // Real backend build must not show the mock banner.
  await expect(page.getByText("Mock 模式")).toHaveCount(0);

  const meRes = await request.get("/api/auth/me");
  expect(meRes.status()).toBe(401);

  const projectsRes = await request.get("/api/projects");
  expect(projectsRes.status()).toBe(401);
});

test("2. 首次管理员使用初始密码登录触发强制改密，改密后需用新密码重新登录", async ({ page }) => {
  await ensureChineseLocale(page);

  await page.goto("/settings/projects");
  await expect(page).toHaveURL(/\/login\?redirect=%2Fsettings%2Fprojects$/);

  await loginViaUI(page, "admin", initialAdminPassword);

  // Forced password change screen outside the AppShell.
  await expect(page).toHaveURL(/\/change-password\?redirect=%2Fsettings%2Fprojects$/);
  await expect(page.getByRole("heading", { name: "请先修改初始密码" })).toBeVisible();
  await expect(page.getByRole("button", { name: "用户菜单" })).toHaveCount(0);

  // Backend refuses normal management APIs with 403 while must_change_password is set.
  const blockedStatus = await page.evaluate(async () => {
    const res = await fetch("/api/projects");
    return res.status;
  });
  expect(blockedStatus).toBe(403);

  const submit = page.getByRole("button", { name: "修改密码" });
  await page.getByLabel("当前密码").fill(initialAdminPassword);
  await page.getByLabel("新密码", { exact: true }).fill("short");
  await expect(page.getByText("至少 8 个字符")).toBeVisible();
  await expect(submit).toBeDisabled();

  await page.getByLabel("新密码", { exact: true }).fill(updatedAdminPassword);
  await page.getByLabel("确认新密码").fill(updatedAdminPassword);
  await expect(submit).toBeEnabled();
  await submit.click();

  // Password changed: redirected back to target inside authenticated AppShell.
  await expect(page).toHaveURL("/settings/projects");
  await expect(page.getByRole("button", { name: "用户菜单" })).toBeVisible();
  await expect(page.getByText("default", { exact: true })).toBeVisible();

  // Backend cleans up the generated initial-admin-password file after rotation.
  await expect.poll(() => existsSync(initialPasswordFile)).toBe(false);

  // Sign out, verify old password is rejected, then log in with the new password.
  await signOutViaTopbar(page);
  await loginViaUI(page, "admin", initialAdminPassword);
  await expect(page.getByRole("alert")).toContainText("账号或密码错误");

  await loginViaUI(page, "admin", updatedAdminPassword);
  await expect(page.getByRole("button", { name: "用户菜单" })).toBeVisible();
  await expect(page).not.toHaveURL(/\/login|\/change-password/);
});

test("3. 项目创建、项目 API Token 仅显示一次与项目作用域切换", async ({ page }) => {
  await ensureChineseLocale(page);

  await page.goto("/settings/projects");
  await expect(page).toHaveURL(/\/login/);
  await loginViaUI(page, "admin", updatedAdminPassword);
  await expect(page).toHaveURL("/settings/projects");

  // Default project exists from bootstrap.
  await expect(page.locator("tbody tr").filter({ hasText: "default" })).toBeVisible();

  // Create a dedicated project for this real-E2E run.
  await page.getByRole("button", { name: "新建项目" }).click();
  const createDialog = page.getByRole("dialog");
  await createDialog.getByLabel("项目名称").fill(projectName);
  await createDialog.getByLabel("标识（slug）").fill(projectSlug);
  await createDialog.getByRole("button", { name: "创建" }).click();
  await expect(createDialog).toHaveCount(0);

  const projectRow = page.locator("tbody tr").filter({ hasText: projectSlug });
  await expect(projectRow).toBeVisible();

  // Issue a project API token and capture its one-time plaintext value.
  await projectRow.getByRole("button", { name: "管理令牌" }).click();
  const tokensDialog = page.getByRole("dialog");
  await expect(tokensDialog.getByText(`${projectName} 的项目 API Token`)).toBeVisible();
  await tokensDialog.getByLabel("API Token 名称").fill(tokenName);
  await tokensDialog.getByRole("button", { name: "创建 API Token" }).click();

  await expect(tokensDialog.getByText("项目 API Token 已创建")).toBeVisible();
  const tokenCode = tokensDialog.locator("code").filter({ hasText: /^lo_/ }).first();
  await expect(tokenCode).toBeVisible();
  issuedProjectToken = (await tokenCode.innerText()).trim();
  expect(issuedProjectToken).toMatch(/^lo_[A-Za-z0-9_-]+$/);

  // Close and reopen the dialog: the full plaintext token must no longer be shown.
  await page.keyboard.press("Escape");
  await expect(tokensDialog).toHaveCount(0);

  await projectRow.getByRole("button", { name: "管理令牌" }).click();
  const reopenedDialog = page.getByRole("dialog");
  await expect(reopenedDialog.getByText(tokenName, { exact: true })).toBeVisible();
  await expect(reopenedDialog.getByText(issuedProjectToken)).toHaveCount(0);
  await expect(reopenedDialog.getByText(/lo_.*…/)).toBeVisible();
  await page.keyboard.press("Escape");

  // Switch active project scope via the sidebar project switcher.
  await switchProjectViaSidebar(page, projectSlug);
  await expect(page.getByRole("button", { name: "切换项目" })).toContainText(projectName);
});

test("4. 真实遥测写入网关并在 Logs / Metrics / Traces / Issues 页面回读闭环", async ({ page, request }) => {
  expect(issuedProjectToken).toMatch(/^lo_/);
  await ensureChineseLocale(page);

  // 1) Ingest real telemetry with the project API token issued in test 3.
  const logBody = encodeOtlpLogRecord({
    body: logMarker,
    serviceName: logService,
    severityNumber: 9,
    severityText: "INFO",
    attributes: { env: "real-e2e", run_id: runId },
    traceIdHex: traceId,
    spanIdHex: spanId,
  });
  const logIngestRes = await request.post("/v1/logs", {
    headers: {
      Authorization: `Bearer ${issuedProjectToken}`,
      "Content-Type": "application/x-protobuf",
    },
    data: Buffer.from(logBody),
  });
  expect(logIngestRes.ok(), `logs ingest failed: ${logIngestRes.status()} ${await logIngestRes.text()}`).toBeTruthy();

  const metricBody = encodeOtlpGaugeMetric({
    name: metricName,
    value: 42,
    resourceAttributes: { "service.name": metricService },
    dataPointAttributes: { env: "real-e2e", route: "/checkout" },
  });
  const metricIngestRes = await request.post("/v1/metrics", {
    headers: {
      Authorization: `Bearer ${issuedProjectToken}`,
      "Content-Type": "application/x-protobuf",
    },
    data: Buffer.from(metricBody),
  });
  expect(
    metricIngestRes.ok(),
    `metrics ingest failed: ${metricIngestRes.status()} ${await metricIngestRes.text()}`,
  ).toBeTruthy();

  const traceIngestRes = await request.post("/v1/traces", {
    headers: {
      Authorization: `Bearer ${issuedProjectToken}`,
      "Content-Type": "application/json",
    },
    data: buildOtlpTracePayload({
      serviceName: traceService,
      traceId,
      spanId,
      operationName: traceOperation,
      attributes: { "http.method": "GET", "http.route": `/real-e2e/checkout/${runId}` },
    }),
  });
  expect(
    traceIngestRes.ok(),
    `traces ingest failed: ${traceIngestRes.status()} ${await traceIngestRes.text()}`,
  ).toBeTruthy();

  const errorIngestRes = await request.post("/api/events/errors", {
    headers: {
      Authorization: `Bearer ${issuedProjectToken}`,
      "Content-Type": "application/json",
    },
    data: buildErrorEventPayload({
      eventId: errorEventId,
      type: errorType,
      value: errorValue,
      message: `${errorType}: ${errorValue}`,
      traceId,
      spanId,
    }),
  });
  expect(
    errorIngestRes.ok(),
    `error ingest failed: ${errorIngestRes.status()} ${await errorIngestRes.text()}`,
  ).toBeTruthy();

  // Log in to the real UI and select our isolated project.
  await page.goto("/logs");
  await expect(page).toHaveURL(/\/login/);
  await loginViaUI(page, "admin", updatedAdminPassword);
  await expect(page.getByRole("button", { name: "用户菜单" })).toBeVisible();
  await switchProjectViaSidebar(page, projectSlug);

  // Wait until VictoriaLogs / VictoriaMetrics / VictoriaTraces / ErrorInbox finish indexing.
  await expect.poll(async () => {
    return page.evaluate(
      async ({ slug, marker, metric, svc, errVal }) => {
        const headers = { "X-LO-Project": slug };
        const [logRes, metricRes, traceRes, issueRes] = await Promise.all([
          fetch(`/api/v1/logs/query?query=${encodeURIComponent(`_msg:="${marker}"`)}&limit=5`, { headers }),
          fetch(`/api/v1/query?query=${encodeURIComponent(metric)}`, { headers }),
          fetch("/api/traces/services", { headers }),
          fetch(`/api/issues?q=${encodeURIComponent(errVal)}`, { headers }),
        ]);
        const logText = logRes.ok ? await logRes.text() : "";
        const metricJson = metricRes.ok
          ? (await metricRes.json()) as { data?: { result?: unknown[] } }
          : null;
        const traceJson = traceRes.ok ? (await traceRes.json()) as { data?: string[] } : null;
        const issueJson = issueRes.ok
          ? (await issueRes.json()) as { issues?: Array<{ title?: string }> }
          : null;
        return {
          logs: logText.includes(marker),
          metrics: (metricJson?.data?.result?.length ?? 0) > 0,
          traces: (traceJson?.data ?? []).includes(svc),
          issues: (issueJson?.issues ?? []).some((item) => item.title?.includes(errVal)),
        };
      },
      { slug: projectSlug, marker: logMarker, metric: metricName, svc: traceService, errVal: errorValue },
    );
  }, { timeout: 30_000, intervals: [500, 1000] }).toEqual({
    logs: true,
    metrics: true,
    traces: true,
    issues: true,
  });

  // 2) Logs page: execute LogsQL query and assert real ingested log renders, plus project isolation.
  await page.goto(`/logs?q=${encodeURIComponent(`_msg:="${logMarker}"`)}&run=1&view=table`);
  await expect(page.getByText(logMarker).first()).toBeVisible();
  await page.getByRole("button", { name: "查看日志详情" }).first().click();
  await expect(page.getByRole("tab", { name: "字段" })).toBeVisible();
  await page.keyboard.press("Escape");

  // Verify project scope isolation: switching to default must not expose real-e2e project's logs.
  await switchProjectViaSidebar(page, "default");
  await expect(page.getByText("无数据").first()).toBeVisible();
  await switchProjectViaSidebar(page, projectSlug);
  await expect(page.getByText(logMarker).first()).toBeVisible();

  // 3) Metrics page: use visual query builder synced with CodeMirror, run PromQL, verify value 42.
  await page.goto("/metrics");
  await page.getByRole("button", { name: "构建器" }).click();
  const metricInput = page.getByPlaceholder("搜索或手动输入指标名…");
  await metricInput.fill(metricName);
  const promqlEditor = page.getByLabel("PromQL 表达式");
  await expect(promqlEditor).toContainText(metricName);

  await page.getByRole("button", { name: "执行查询" }).click();
  await expect(page.getByText(/加载耗时：\d+ ms\u3000结果序列：1/)).toBeVisible();

  await page.getByRole("tab", { name: "表格" }).click();
  await expect(page.locator("main")).toContainText("42");
  await expect(page.locator("main")).toContainText("real-e2e");

  // 4) Traces page: search ingested service, verify trace list and open waterfall detail.
  await page.goto("/traces");
  const searchBtn = page.getByRole("button", { name: "检索" });
  await expect(searchBtn).toBeEnabled();
  await expect(page.locator("main")).toContainText(traceService);
  await searchBtn.click();
  await expect(page.getByText("1 条链路")).toBeVisible();
  await expect(page.getByText(traceOperation)).toBeVisible();

  await page.locator('main [role="button"][tabindex="0"]').first().click();
  await expect(page).toHaveURL(new RegExp(`/traces/${traceId}$`));
  await expect(page.locator("main")).toContainText(`Trace ID:${traceId}`);
  await expect(page.getByText(traceOperation).first()).toBeVisible();

  // 5) Issues page: verify aggregated error issue renders and opens detail with stacktrace.
  await page.goto("/issues");
  const issueButton = page.locator("main ul li button").filter({ hasText: errorValue }).first();
  await expect(issueButton).toBeVisible();
  await issueButton.click();
  await expect(page).toHaveURL(/\/issues\/[^/?]+/);
  await expect(page.getByText("堆栈跟踪", { exact: true })).toBeVisible();
  await expect(page.getByText("submitRealE2EOrder").first()).toBeVisible();
});

test("5. 告警抑制规则（/alerts/inhibitions）真实 CRUD 与持久化闭环，以及登出边界", async ({ page }) => {
  await ensureChineseLocale(page);

  await page.goto("/alerts/inhibitions");
  await expect(page).toHaveURL(/\/login/);
  await loginViaUI(page, "admin", updatedAdminPassword);
  await expect(page).toHaveURL("/alerts/inhibitions");
  await switchProjectViaSidebar(page, projectSlug);

  // Empty state before creation.
  await expect(page.getByText("暂无抑制规则")).toBeVisible();

  // Create a real inhibition rule.
  await page.getByRole("button", { name: "新建抑制规则" }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByRole("heading", { name: "新建抑制规则" })).toBeVisible();

  const nameInputs = dialog.getByPlaceholder("标签名");
  const valueInputs = dialog.getByPlaceholder("值");
  await nameInputs.nth(0).fill("severity");
  await valueInputs.nth(0).fill("critical");
  await nameInputs.nth(1).fill("severity");
  await valueInputs.nth(1).fill("warning");

  const equalLabelInput = dialog.getByLabel("相同标签");
  await equalLabelInput.fill("service");
  await equalLabelInput.press("Enter");
  await expect(dialog.getByText("service", { exact: true })).toBeVisible();

  await dialog.getByLabel("备注").fill(inhibitionComment);
  await dialog.getByRole("button", { name: "创建" }).click();
  await expect(dialog).toHaveCount(0);

  // Verify created card appears in enabled section and persists across a full reload.
  await expect(page.getByText(inhibitionComment)).toBeVisible();
  await expect(page.getByText("已启用", { exact: true }).first()).toBeVisible();

  await page.reload();
  await expect(page.getByText(inhibitionComment)).toBeVisible();
  await expect(page.getByText("critical", { exact: true })).toBeVisible();
  await expect(page.getByText("warning", { exact: true })).toBeVisible();
  await expect(page.getByText("service", { exact: true })).toBeVisible();

  // Toggle enabled switch -> moves to disabled section and persists across reload.
  const toggleSwitch = page.getByRole("switch", { name: "启用规则" }).first();
  await expect(toggleSwitch).toBeChecked();
  await toggleSwitch.click();
  await expect(page.getByText("已停用", { exact: true })).toBeVisible();
  await expect(page.getByText(/^已禁用\s*1$/)).toBeVisible();
  await expect(toggleSwitch).not.toBeChecked();

  await page.reload();
  await expect(page.getByText(inhibitionComment)).toBeVisible();
  await expect(page.getByText("已停用", { exact: true })).toBeVisible();
  await expect(page.getByText(/^已禁用\s*1$/)).toBeVisible();
  await expect(page.getByRole("switch", { name: "启用规则" }).first()).not.toBeChecked();

  // Delete rule -> list returns to empty state and stays empty after reload.
  await page.getByRole("button", { name: "删除抑制规则" }).first().click();
  const confirmDialog = page.getByRole("dialog");
  await expect(confirmDialog.getByText("确认删除该抑制规则？")).toBeVisible();
  await confirmDialog.getByRole("button", { name: "删除抑制规则" }).click();
  await expect(confirmDialog).toHaveCount(0);
  await expect(page.getByText("暂无抑制规则")).toBeVisible();

  await page.reload();
  await expect(page.getByText("暂无抑制规则")).toBeVisible();

  // Sign out and verify session revocation: protected APIs return 401 and protected routes redirect to /login.
  await signOutViaTopbar(page);
  const apiStatusAfterLogout = await page.evaluate(async () => {
    const [me, projects] = await Promise.all([
      fetch("/api/auth/me"),
      fetch("/api/projects"),
    ]);
    return { me: me.status, projects: projects.status };
  });
  expect(apiStatusAfterLogout).toEqual({ me: 401, projects: 401 });

  await page.goto("/alerts/inhibitions");
  await expect(page).toHaveURL(/\/login\?redirect=%2Falerts%2Finhibitions$/);
});

if (selfMonitoringEnabled) {
  test("6. 真实后端 Web 自观测闭环（登录后激活、三信号回读、登出清理与禁用开关）", async ({ page, request }) => {
    const ingestPaths = new Set([
      "/v1/traces",
      "/api/events/vitals",
      "/api/events/errors",
      "/api/events/diagnostics",
      "/api/replay/segments",
    ]);
    const isIngestPath = (rawURL: string): string | null => {
      try {
        const pathname = new URL(rawURL).pathname;
        if (pathname.startsWith("/p/") || ingestPaths.has(pathname)) {
          return pathname;
        }
        return null;
      } catch {
        return null;
      }
    };

    interface BootstrapEvent {
      status: number;
      body: {
        enabled?: boolean;
        project_slug?: string;
        token?: string;
        service_name?: string;
        replay_enabled?: boolean;
      } | null;
    }
    interface IngestEvent {
      phase: string;
      pathname: string;
      status?: number;
      postData: string;
    }

    let phase = "unauth";
    const bootstrapEvents: BootstrapEvent[] = [];
    const ingestRequests: IngestEvent[] = [];
    const ingestResponses: IngestEvent[] = [];

    page.on("pageerror", () => {
      // Expected unhandled errors are triggered deliberately to verify capture.
    });
    page.on("request", (req) => {
      if (req.method() !== "POST") return;
      const pathname = isIngestPath(req.url());
      if (!pathname) return;
      ingestRequests.push({
        phase,
        pathname,
        postData: req.postData() ?? "",
      });
    });
    page.on("response", async (resp) => {
      const req = resp.request();
      let pathname: string;
      try {
        pathname = new URL(resp.url()).pathname;
      } catch {
        return;
      }
      if (req.method() === "GET" && pathname === "/api/system/self-monitoring") {
        let body: BootstrapEvent["body"];
        try {
          body = (await resp.json()) as BootstrapEvent["body"];
        } catch {
          body = null;
        }
        bootstrapEvents.push({ status: resp.status(), body });
        return;
      }
      if (req.method() === "POST" && isIngestPath(resp.url())) {
        ingestResponses.push({
          phase,
          pathname,
          status: resp.status(),
          postData: req.postData() ?? "",
        });
      }
    });

    await ensureChineseLocale(page);
    await page.goto("/login");
    await expect(page.locator("#username")).toBeVisible();
    const loginHtml = await page.content();
    expect(/\blo_[A-Za-z0-9_-]{16,}\b/.test(loginHtml)).toBe(false);

    const unauthStatus = await page.evaluate(async () => {
      const resp = await fetch("/api/system/self-monitoring", { credentials: "same-origin" });
      return resp.status;
    });
    expect(unauthStatus).toBe(401);

    phase = "authed";
    await loginViaUI(page, "admin", updatedAdminPassword);
    await expect(page.getByRole("button", { name: "用户菜单" })).toBeVisible();

    await expect.poll(() => bootstrapEvents.some((item) => item.status === 200)).toBe(true);
    const activeBootstrap = bootstrapEvents.find((item) => item.status === 200);
    expect(activeBootstrap).toBeDefined();

    if (selfMonitoringWebDisabled) {
      expect(activeBootstrap?.body).toEqual({ enabled: false });
      await page.waitForTimeout(1_000);
      expect(
        await page.evaluate(() => window.sessionStorage.getItem("lazy-observe:session-id")),
      ).toBeNull();

      await page.locator('a[href="/logs"]').first().click();
      await expect(page).toHaveURL(/\/logs/);
      await page.evaluate(async (marker) => {
        await fetch("/api/projects", { credentials: "same-origin" });
        setTimeout(() => {
          throw new Error(marker);
        }, 0);
      }, `real-e2e-web-self-mon-disabled-${runId}`);

      await page.waitForTimeout(6_500);
      expect(ingestRequests).toEqual([]);
      return;
    }

    expect(activeBootstrap?.body?.enabled).toBe(true);
    expect(activeBootstrap?.body?.project_slug).toBe("lazy-observe");
    expect(activeBootstrap?.body?.service_name).toBe("lazy-observe-web");
    expect(activeBootstrap?.body?.token).toMatch(/^lo_/);
    expect(loginHtml).not.toContain(activeBootstrap?.body?.token ?? "");

    await expect.poll(() =>
      page.evaluate(() => window.sessionStorage.getItem("lazy-observe:session-id")),
    ).not.toBeNull();

    await page.locator('a[href="/logs"]').first().click();
    await expect(page).toHaveURL(/\/logs/);
    const selfMonErrorMarker = `real-e2e-web-self-mon-${runId}`;
    await page.evaluate(async (marker) => {
      await fetch("/api/projects", { credentials: "same-origin" });
      setTimeout(() => {
        throw new Error(marker);
      }, 0);
    }, selfMonErrorMarker);

    await expect.poll(() => {
      const okResponses = ingestResponses.filter(
        (item) => item.phase === "authed" && (item.status ?? 0) >= 200 && (item.status ?? 0) < 300,
      );
      return {
        trace: okResponses.some(
          (item) => item.pathname === "/v1/traces" && item.postData.includes("lazy-observe-web"),
        ),
        vital: okResponses.some(
          (item) => item.pathname === "/api/events/vitals" && /"(TTFB|FCP|LCP|CLS|INP)"/.test(item.postData),
        ),
        error: okResponses.some(
          (item) => item.pathname === "/api/events/errors" && item.postData.includes(selfMonErrorMarker),
        ),
      };
    }, { timeout: 30_000, intervals: [300, 500] }).toEqual({
      trace: true,
      vital: true,
      error: true,
    });

    const loginRes = await request.post("/api/auth/login", {
      data: { username: "admin", password: updatedAdminPassword },
    });
    expect(loginRes.ok()).toBeTruthy();

    await expect.poll(async () => {
      const headers = { "X-LO-Project": "lazy-observe" };
      const [svcRes, searchRes, vitalRes, issueRes] = await Promise.all([
        request.get("/api/traces/services", { headers }),
        request.get("/api/traces/search", {
          headers,
          params: { service: "lazy-observe-web", limit: "20" },
        }),
        request.get("/api/v1/query", {
          headers,
          params: { query: `{__name__=~"web_vitals_(lcp|inp|cls|ttfb|fcp)"}` },
        }),
        request.get("/api/issues", {
          headers,
          params: { q: selfMonErrorMarker },
        }),
      ]);
      const svcJson = svcRes.ok() ? ((await svcRes.json()) as { data?: string[] | null }) : null;
      const searchJson = searchRes.ok() ? ((await searchRes.json()) as { data?: unknown[] | null }) : null;
      const vitalJson = vitalRes.ok()
        ? ((await vitalRes.json()) as { data?: { result?: unknown[] } })
        : null;
      const issueJson = issueRes.ok()
        ? ((await issueRes.json()) as { issues?: Array<{ title?: string }> })
        : null;
      return {
        traceService: (svcJson?.data ?? []).includes("lazy-observe-web"),
        traces: (searchJson?.data?.length ?? 0) > 0,
        vitals: (vitalJson?.data?.result?.length ?? 0) > 0,
        issues: (issueJson?.issues ?? []).some((item) => item.title?.includes(selfMonErrorMarker)),
      };
    }, { timeout: 45_000, intervals: [500, 1_000] }).toEqual({
      traceService: true,
      traces: true,
      vitals: true,
      issues: true,
    });

    await page.evaluate(() => {
      window.sessionStorage.setItem("lazy-observe:replay-seq:real-e2e", "1");
    });
    await signOutViaTopbar(page);
    const remainingKeys = await page.evaluate(() => Object.keys(window.sessionStorage));
    expect(remainingKeys).not.toContain("lazy-observe:session-id");
    expect(remainingKeys.some((key) => key.startsWith("lazy-observe:replay-seq:"))).toBe(false);

    await page.waitForTimeout(1_000);
    phase = "post-logout";
    const postLogoutErrorMarker = `real-e2e-web-self-mon-after-logout-${runId}`;
    await page.evaluate(async (marker) => {
      await fetch("/healthz", { credentials: "same-origin" });
      setTimeout(() => {
        throw new Error(marker);
      }, 0);
    }, postLogoutErrorMarker);

    await page.waitForTimeout(6_500);
    expect(ingestRequests.filter((item) => item.phase === "post-logout")).toEqual([]);
  });
}
