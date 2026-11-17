import { expect, test, type APIRequestContext, type Page } from "@playwright/test";

const platformURL = requiredEnv("LO_E2E_PLATFORM_URL");
const goURL = requiredEnv("LO_E2E_GO_URL");
const nodeURL = requiredEnv("LO_E2E_NODE_URL");
const password = requiredEnv("LO_E2E_ADMIN_PASSWORD");

interface EndpointStat {
  service: string;
  method: string;
  route: string;
  p95: number;
  error_rate: number;
}

interface EndpointResponse {
  start: number;
  end: number;
  endpoints: EndpointStat[];
}

interface ProfileRecord {
  service: string;
  type: "cpu" | "heap" | "goroutine";
}

interface FlameNode {
  name: string;
  value: number;
  children?: FlameNode[];
}

function requiredEnv(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required`);
  return value;
}

// Project API tokens are ingest-only (ADR 0010); every read below rides on
// this session cookie plus X-LO-Project.
async function loginAPI(request: APIRequestContext): Promise<void> {
  const response = await request.post(`${platformURL}/api/auth/login`, {
    data: { username: "admin", password },
  });
  expect(response.ok()).toBeTruthy();
}

async function loginUI(page: Page, target: string): Promise<void> {
  await page.goto(target);
  const loginHeading = page.getByRole("heading", { name: /Sign in|登录/ });
  const targetHeading = page.getByRole("heading", { name: /Performance|性能/ });
  await expect(loginHeading.or(targetHeading)).toBeVisible();
  if (await loginHeading.isVisible()) {
    await page.getByLabel(/Username|用户名/).fill("admin");
    await page.getByLabel(/Password|密码/).fill(password);
    await page.getByRole("button", { name: /Sign in|登录/ }).click();
    await expect(targetHeading).toBeVisible();
  }
}

async function performanceContract(request: APIRequestContext): Promise<EndpointResponse> {
  let result: EndpointResponse | undefined;
  await expect.poll(async () => {
    const end = Date.now();
    const response = await request.get(`${platformURL}/api/v1/performance/endpoints`, {
      headers: { "X-LO-Project": "default" },
      params: {
        start: new Date(end - 60 * 60 * 1_000).toISOString(),
        end: new Date(end).toISOString(),
        step: "30",
      },
    });
    if (!response.ok()) return "";
    result = await response.json() as EndpointResponse;
    const routes = result.endpoints
      .map((endpoint) => `${endpoint.service}:${endpoint.route}`)
      .sort();
    const required = [
      "lazy-observe-fiber-example:/performance/fast",
      "lazy-observe-fiber-example:/performance/slow",
      "lazy-observe-fiber-example:/performance/error",
      "node-express-example:/performance/slow",
    ];
    return required.every((route) => routes.includes(route)) ? "ready" : routes.join("\n");
  }, { timeout: 60_000 }).toBe("ready");
  expect(result!.endpoints.map((endpoint) => `${endpoint.service}:${endpoint.route}`))
    .toContain("node-express-example:/performance/slow");
  return result!;
}

async function waitForTraceAtP95(
  request: APIRequestContext,
  response: EndpointResponse,
  endpoint: EndpointStat,
): Promise<void> {
  await expect.poll(async () => {
    const result = await request.get(`${platformURL}/api/traces/search`, {
      headers: { "X-LO-Project": "default" },
      params: {
        service: endpoint.service,
        operation: `${endpoint.method} ${endpoint.route}`,
        start: String(response.start * 1_000),
        end: String(response.end * 1_000),
        minDuration: `${Math.ceil(endpoint.p95 * 1_000)}us`,
        limit: "20",
      },
    });
    if (!result.ok()) return 0;
    const body = await result.json() as { data?: unknown[] };
    return body.data?.length ?? 0;
  }, { timeout: 60_000 }).toBeGreaterThan(0);
}

async function waitForProfileTypes(
  request: APIRequestContext,
  service: string,
  wanted: string[],
): Promise<void> {
  await expect.poll(async () => {
    const response = await request.get(`${platformURL}/api/profiles`, {
      headers: { "X-LO-Project": "default" },
      params: { service },
    });
    if (!response.ok()) return [];
    const body = await response.json() as { profiles?: ProfileRecord[] };
    return [...new Set((body.profiles ?? []).map((profile) => profile.type))].sort();
  }, { timeout: 60_000 }).toEqual([...wanted].sort());
}

async function flamegraph(
  request: APIRequestContext,
  service: string,
  type: ProfileRecord["type"],
): Promise<FlameNode> {
  const response = await request.get(`${platformURL}/api/profiles/flamegraph`, {
    headers: { "X-LO-Project": "default" },
    params: { service, type },
  });
  expect(response.ok()).toBeTruthy();
  return await response.json() as FlameNode;
}

function hasFlameNode(root: FlameNode, name: string): boolean {
  return root.name.includes(name) || (root.children ?? []).some((child) => hasFlameNode(child, name));
}

test("performance investigation journey uses ranked endpoints and contextual evidence", async ({
  page,
  request,
}) => {
  test.setTimeout(180_000);
  expect(goURL).toMatch(/^http:\/\//);
  expect(nodeURL).toMatch(/^http:\/\//);
  await loginAPI(request);

  const contract = await performanceContract(request);
  const goFast = contract.endpoints.find((endpoint) => (
    endpoint.service === "lazy-observe-fiber-example" && endpoint.route === "/performance/fast"
  ));
  const goSlow = contract.endpoints.find((endpoint) => (
    endpoint.service === "lazy-observe-fiber-example" && endpoint.route === "/performance/slow"
  ));
  const goError = contract.endpoints.find((endpoint) => (
    endpoint.service === "lazy-observe-fiber-example" && endpoint.route === "/performance/error"
  ));
  expect(goFast).toBeDefined();
  expect(goSlow).toBeDefined();
  expect(goError).toBeDefined();
  expect(goSlow!.p95).toBeGreaterThan(goFast!.p95);
  expect(goError!.error_rate).toBeGreaterThan(0);
  await waitForTraceAtP95(request, contract, goSlow!);

  await waitForProfileTypes(
    request,
    "lazy-observe-fiber-example",
    ["cpu", "heap", "goroutine"],
  );
  await waitForProfileTypes(request, "node-express-example", ["cpu", "heap"]);
  expect(hasFlameNode(await flamegraph(request, "lazy-observe-fiber-example", "cpu"), "main.cpuWork"))
    .toBe(true);
  expect((await flamegraph(request, "lazy-observe-fiber-example", "heap")).value).toBeGreaterThan(0);
  expect((await flamegraph(request, "lazy-observe-fiber-example", "goroutine")).value).toBeGreaterThan(0);
  expect(hasFlameNode(await flamegraph(request, "node-express-example", "cpu"), "profileExampleWork"))
    .toBe(true);
  expect((await flamegraph(request, "node-express-example", "heap")).value).toBeGreaterThan(0);

  await loginUI(page, `${platformURL}/performance`);
  await expect(page.getByRole("heading", { name: /Performance|性能/ })).toBeVisible();
  await expect(page.getByRole("button", { name: /Largest Contentful Paint|最大内容绘制/ })).toBeVisible();
  const endpointResponse = page.waitForResponse((response) => (
    new URL(response.url()).pathname === "/api/v1/performance/endpoints" && response.ok()
  ));
  await page.getByRole("tab", { name: /Backend endpoints|后端接口/ }).click();
  const uiContract = await (await endpointResponse).json() as EndpointResponse;
  const uiSlow = uiContract.endpoints.find((endpoint) => (
    endpoint.service === "lazy-observe-fiber-example" && endpoint.route === "/performance/slow"
  ));
  expect(uiSlow).toBeDefined();
  await waitForTraceAtP95(request, uiContract, uiSlow!);

  const endpointRow = page.getByRole("link", {
    name: /GET \/performance\/slow.*lazy-observe-fiber-example|lazy-observe-fiber-example.*GET \/performance\/slow/,
  });
  await expect(endpointRow).toBeVisible();
  const retainedURL = page.url();
  let failNextEndpointRefresh = true;
  await page.route("**/api/v1/performance/endpoints?**", async (route) => {
    if (!failNextEndpointRefresh) {
      await route.continue();
      return;
    }
    failNextEndpointRefresh = false;
    await route.fulfill({
      status: 500,
      contentType: "application/json",
      body: JSON.stringify({ error: "private http://127.0.0.1:10428" }),
    });
  });
  await page.getByRole("button", { name: /^Refresh$|^刷新$/ }).click();
  const staleNotice = page.getByTestId("query-stale-notice");
  await expect(staleNotice).toBeVisible();
  await expect(endpointRow).toBeVisible();
  await expect(page.getByText("127.0.0.1", { exact: false })).toHaveCount(0);
  expect(page.url()).toBe(retainedURL);

  const recoveredResponse = page.waitForResponse((response) => (
    new URL(response.url()).pathname === "/api/v1/performance/endpoints" && response.ok()
  ));
  await staleNotice.getByRole("button", { name: /^Retry$|^重试$/ }).click();
  await recoveredResponse;
  await expect(staleNotice).toHaveCount(0);
  await expect(endpointRow).toBeVisible();
  expect(page.url()).toBe(retainedURL);
  await page.unroute("**/api/v1/performance/endpoints?**");

  await endpointRow.click();
  const traceURL = new URL(page.url());
  expect(traceURL.pathname).toBe("/traces");
  expect(traceURL.searchParams.get("service")).toBe("lazy-observe-fiber-example");
  expect(traceURL.searchParams.get("operation")).toBe("GET /performance/slow");
  expect(traceURL.searchParams.get("start")).toBe(String(uiContract.start * 1_000));
  expect(traceURL.searchParams.get("end")).toBe(String(uiContract.end * 1_000));
  expect(traceURL.searchParams.get("minDuration")).toBe(`${Math.ceil(uiSlow!.p95 * 1_000)}us`);
  await expect(page.getByText(/\d+ traces|\d+ 条链路/)).toBeVisible();
  await page.locator('main [role="button"][tabindex="0"]').first().click();
  await expect(page).toHaveURL(/\/traces\/[a-f0-9]{32}$/);
  const traceDetailURL = page.url();

  const rootSpanRow = page.locator('main [role="button"][tabindex="0"]')
    .filter({ hasText: "GET /performance/slow" })
    .first();
  await expect(rootSpanRow).toBeVisible();
  await rootSpanRow.click();
  await page.getByRole("button", { name: /View trace logs|查看 Trace 精确日志/ }).click();
  await expect(page).toHaveURL(/\/logs\?/);
  await expect(page.getByText("performance workload complete", { exact: false }).first()).toBeVisible();

  await page.goto(traceDetailURL);
  await expect(page.getByText(/Contextual evidence|同期上下文证据/).first()).toBeVisible();
  await page.getByRole("button", { name: /View profiling for this window|查看该时段剖析/ }).click();
  await expect(page).toHaveURL(/\/profiling\?/);
  await expect(page.getByRole("note")).toContainText(/not measurements of this individual request|并非此单次请求/);
  const profileSearch = page.getByPlaceholder(/Search function name|搜索函数名/);
  await profileSearch.fill("main.cpuWork");
  await expect(page.getByText(/[1-9][0-9]* (matches|处匹配)/)).toBeVisible();

  await page.getByRole("combobox").first().click();
  await page.getByRole("option", { name: "node-express-example" }).click();
  await profileSearch.fill("profileExampleWork");
  await expect(page.getByText(/[1-9][0-9]* (matches|处匹配)/)).toBeVisible();

  await page.goto(traceDetailURL);
  await page.getByRole("button", { name: /View metrics for this window|查看该时段指标/ }).click();
  await expect(page).toHaveURL(/\/metrics\?/);
  await expect(page.getByRole("note")).toContainText(/not measurements of this individual request|并非此单次请求/);
  await expect(page.getByLabel(/PromQL expression|PromQL 表达式/))
    .toContainText("http.server.request.duration_count");
  await expect(page.getByText(/Load time:|加载耗时：/)).toBeVisible();
});
