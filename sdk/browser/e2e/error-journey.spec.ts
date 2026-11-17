import { expect, test, type APIRequestContext } from "@playwright/test";

const platformURL = requiredEnv("LO_E2E_PLATFORM_URL");
const reactURL = requiredEnv("LO_E2E_REACT_URL");
const token = requiredEnv("LO_E2E_TOKEN");
const password = requiredEnv("LO_E2E_ADMIN_PASSWORD");
const release = requiredEnv("LO_E2E_ERROR_JOURNEY_RELEASE");
const dsn = `${platformURL}/p/default@${token}`;

interface IssueSummary {
  id: number;
  title: string;
  count: number;
  status: "open" | "resolved" | "ignored";
  regressed_at?: number;
}

interface IssueEvent {
  event_id: string;
  release: string;
  exception: {
    stacktrace: { frames: StackFrame[] };
    raw_stacktrace?: { frames: StackFrame[] };
  };
  contexts: { trace?: { trace_id: string; span_id?: string } };
}

interface StackFrame {
  function: string;
  filename: string;
  lineno?: number;
  colno?: number;
}

interface JaegerSpan {
  traceID: string;
  spanID: string;
  operationName: string;
  references: Array<{ refType: string; spanID: string }>;
}

interface JaegerTrace {
  traceID: string;
  spans: JaegerSpan[];
}

interface ReplayDetail {
  session_id: string;
  segments: Array<{ seq: number }>;
  markers: Array<{
    type: "error" | "http" | "vital";
    issue_id?: string;
    event_id?: string;
    path?: string;
    name?: string;
  }>;
}

function requiredEnv(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required`);
  return value;
}

async function login(request: APIRequestContext): Promise<void> {
  const response = await request.post(`${platformURL}/api/auth/login`, {
    data: { username: "admin", password },
  });
  expect(response.ok()).toBeTruthy();
}

async function waitForIssueEvent(
  request: APIRequestContext,
  query: string,
  eventID: string,
): Promise<{ issue: IssueSummary; event: IssueEvent }> {
  let issue: IssueSummary | undefined;
  await expect.poll(async () => {
    const response = await request.get(`${platformURL}/api/issues`, {
      headers: { "X-LO-Project": "default" },
      params: { q: query },
    });
    if (!response.ok()) return 0;
    const body = await response.json() as { issues?: IssueSummary[] };
    issue = body.issues?.find((candidate) => candidate.title.includes(query));
    return issue?.id ?? 0;
  }, { timeout: 30_000 }).toBeGreaterThan(0);

  let event: IssueEvent | undefined;
  await expect.poll(async () => {
    const response = await request.get(`${platformURL}/api/issues/${issue!.id}/events/${eventID}`, {
      headers: { "X-LO-Project": "default" },
    });
    if (!response.ok()) return "";
    event = await response.json() as IssueEvent;
    return event.event_id;
  }, { timeout: 30_000 }).toBe(eventID);
  return { issue: issue!, event: event! };
}

async function issueState(request: APIRequestContext, issueID: number): Promise<IssueSummary> {
  const response = await request.get(`${platformURL}/api/issues/${issueID}`, {
    headers: { "X-LO-Project": "default" },
  });
  expect(response.ok()).toBeTruthy();
  return await response.json() as IssueSummary;
}

test.describe.configure({ mode: "serial" });

test("error investigation journey uses production source maps, Trace, and Logs", async ({ page, request }) => {
  test.setTimeout(120_000);
  const secret = "card-secret-must-never-appear";
  const uiPage = await page.context().newPage();
  await page.goto(`${reactURL}/?dsn=${encodeURIComponent(dsn)}`);
  await expect(page.getByRole("heading", { name: "lazy-observe React demo" })).toBeVisible();

  await page.getByLabel("Payment secret").fill(secret);

  await page.getByRole("button", { name: "Run error journey" }).click();
  const result = page.getByLabel("Error journey result");
  await expect(result).toContainText(/^Event: [0-9a-f-]{36}\nTrace: [0-9a-f]{32}$/);
  const resultText = await result.innerText();
  const eventID = resultText.match(/Event: ([0-9a-f-]{36})/)?.[1] ?? "";
  const traceID = resultText.match(/Trace: ([0-9a-f]{32})/)?.[1] ?? "";
  const sessionID = await page.getByLabel("Session ID").innerText();
  expect(eventID).not.toBe("");
  expect(traceID).not.toBe("");
  expect(sessionID).not.toBe("");

  await login(request);
  const { issue, event } = await waitForIssueEvent(request, "Checkout request failed", eventID);
  expect(event.release).toBe(release);
  expect(event.contexts.trace?.trace_id).toBe(traceID);
  expect(event.exception.stacktrace.frames).toEqual(expect.arrayContaining([
    expect.objectContaining({
      function: expect.stringMatching(/createCheckoutFailure/),
      filename: expect.stringMatching(/src\/main\.tsx$/),
      lineno: expect.any(Number),
      colno: expect.any(Number),
    }),
  ]));
  expect(event.exception.stacktrace.frames.some((frame) => /\/assets\/.*\.js$/.test(frame.filename))).toBe(false);
  expect(event.exception.raw_stacktrace?.frames.some((frame) => /\/assets\/.*\.js$/.test(frame.filename))).toBe(true);

  let trace: JaegerTrace | undefined;
  await expect.poll(async () => {
    const response = await request.get(`${platformURL}/api/traces/${traceID}`, {
      headers: { Authorization: `Bearer ${token}` },
    });
    if (!response.ok()) return [];
    const body = await response.json() as { data?: JaegerTrace[] };
    trace = body.data?.[0];
    return trace?.spans.map((span) => span.operationName) ?? [];
  }, { timeout: 30_000 }).toEqual(expect.arrayContaining([
    "submit checkout",
    "GET /journey-api/performance/error",
    "GET /performance/error",
  ]));

  const action = trace!.spans.find((span) => span.operationName === "submit checkout")!;
  const browserRequest = trace!.spans.find((span) => span.operationName === "GET /journey-api/performance/error")!;
  const serverRequest = trace!.spans.find((span) => span.operationName === "GET /performance/error")!;
  expect(browserRequest.references).toContainEqual(expect.objectContaining({ spanID: action.spanID }));
  expect(serverRequest.references).toContainEqual(expect.objectContaining({ spanID: browserRequest.spanID }));

  await expect.poll(async () => {
    const response = await request.get(`${platformURL}/api/v1/logs/query`, {
      headers: { Authorization: `Bearer ${token}` },
      params: { query: `_msg:="performance workload complete"`, limit: 50 },
    });
    return response.ok() ? await response.text() : "";
  }, { timeout: 30_000 }).toContain(traceID);

  await uiPage.goto(`${platformURL}/issues/${issue.id}?event=${eventID}`);
  await expect(uiPage.getByRole("heading", { name: /Sign in|登录/ })).toBeVisible();
  await uiPage.getByLabel(/Username|用户名/).fill("admin");
  await uiPage.getByLabel(/Password|密码/).fill(password);
  await uiPage.getByRole("button", { name: /Sign in|登录/ }).click();
  await expect(uiPage.getByRole("heading", { name: /Checkout request failed/ })).toBeVisible();
  await expect(uiPage.getByText(/createCheckoutFailure/).first()).toBeVisible();
  await expect(uiPage.getByText(/src\/main\.tsx/).first()).toBeVisible();
  await expect(uiPage.getByText(/Total events|事件总数/).locator("..")).toContainText("1");

  await uiPage.getByRole("button", { name: /Resolve|解决/ }).click();
  await expect(uiPage.getByRole("button", { name: /Reopen|重新打开/ })).toBeVisible();

  const firstResult = await result.innerText();
  await page.getByRole("button", { name: "Run error journey" }).click();
  await expect(result).not.toHaveText(firstResult);
  await expect.poll(async () => {
    const state = await issueState(request, issue.id);
    return `${state.status}:${state.count}:${state.regressed_at !== undefined}`;
  }, { timeout: 30_000 }).toBe("open:2:true");
  await uiPage.goto(`${platformURL}/issues/${issue.id}?event=${eventID}`);
  await expect(uiPage.getByText(/Total events|事件总数/).locator("..")).toContainText("2");
  await expect(uiPage.getByRole("button", { name: /Resolve|解决/ })).toBeVisible();

  await uiPage.getByRole("button", { name: /Ignore|忽略/ }).click();
  await expect(uiPage.getByRole("button", { name: /Reopen|重新打开/ })).toBeVisible();
  const secondResult = await result.innerText();
  await page.getByRole("button", { name: "Run error journey" }).click();
  await expect(result).not.toHaveText(secondResult);
  await expect.poll(async () => {
    const state = await issueState(request, issue.id);
    return `${state.status}:${state.count}`;
  }, { timeout: 30_000 }).toBe("ignored:3");
  await uiPage.goto(`${platformURL}/issues/${issue.id}?event=${eventID}`);
  await expect(uiPage.getByText(/Total events|事件总数/).locator("..")).toContainText("3");
  await expect(uiPage.getByRole("button", { name: /Reopen|重新打开/ })).toBeVisible();

  let replay: ReplayDetail | undefined;
  await expect.poll(async () => {
    const response = await request.get(`${platformURL}/api/replay/sessions/${sessionID}`, {
      headers: { "X-LO-Project": "default" },
    });
    if (!response.ok()) return "";
    replay = await response.json() as ReplayDetail;
    const counts = { error: 0, http: 0, vital: 0 };
    for (const marker of replay.markers) counts[marker.type] += 1;
    return `${counts.error}:${counts.http}:${counts.vital}`;
  }, { timeout: 30_000 }).toMatch(/^3:3:[1-9][0-9]*$/);
  expect(replay!.markers.filter((marker) => marker.type === "error"))
    .toEqual(expect.arrayContaining([
      expect.objectContaining({ event_id: eventID, issue_id: String(issue.id) }),
    ]));
  expect(replay!.markers.filter((marker) => marker.type === "http").every((marker) =>
    marker.path === "/journey-api/performance/error"
  )).toBe(true);

  const rawSegments = await Promise.all(replay!.segments.map(async ({ seq }) => {
    const response = await request.get(`${platformURL}/api/replay/sessions/${sessionID}/segments/${seq}`, {
      headers: { "X-LO-Project": "default" },
    });
    expect(response.ok()).toBeTruthy();
    return await response.text();
  }));
  const rawReplay = rawSegments.join("\n");
  expect(rawReplay).toContain("lazy-observe:error");
  expect(rawReplay).not.toContain(secret);

  await uiPage.getByRole("button", { name: /Play replay|播放回放/ }).click();
  await expect(uiPage).toHaveURL(new RegExp(`/replays/${sessionID}$`));
  await expect(uiPage.getByRole("button", { name: /(?:Errors|错误) \(3\)/ })).toBeVisible();
  await expect(uiPage.getByRole("button", { name: /(?:Requests|请求) \(3\)/ })).toBeVisible();
  await expect(uiPage.getByRole("button", { name: /(?:Web Vitals) \([1-9][0-9]*\)/ })).toBeVisible();
  await expect(uiPage.getByText("GET /journey-api/performance/error", { exact: false }).first()).toBeVisible();
  const replayError = uiPage.getByRole("button", { name: /Checkout request failed with status 503/ }).first();
  await replayError.click();
  const replayFrame = uiPage.frameLocator(".lo-rrweb iframe");
  await expect(replayFrame.getByLabel("Payment secret")).toBeVisible();
  await expect(replayFrame.getByLabel("Payment secret")).not.toHaveValue(secret);
  await expect(uiPage.getByText(secret, { exact: false })).toHaveCount(0);
  await uiPage.getByRole("button", { name: /Open issue|查看问题/ }).first().click();
  await expect(uiPage).toHaveURL(new RegExp(`/issues/${issue.id}\\?event=`));

  await uiPage.goto(`${platformURL}/issues/${issue.id}?event=${eventID}`);
  await uiPage.getByRole("button", { name: /View trace|查看链路/ }).click();
  await expect(uiPage).toHaveURL(new RegExp(`/traces/${traceID}$`));
  await expect(uiPage.getByText("submit checkout", { exact: true }).first()).toBeVisible();
  await expect(uiPage.getByText("GET /performance/error", { exact: true }).first()).toBeVisible();
  await uiPage.getByText("GET /performance/error", { exact: true }).first().click();
  await uiPage.getByRole("button", { name: /View trace logs|查看 Trace 精确日志/ }).click();
  await expect(uiPage).toHaveURL(/\/logs\?/);
  expect(decodeURIComponent(uiPage.url())).toContain(traceID);

  await page.goto(`${reactURL}/?dsn=${encodeURIComponent(dsn)}`);
  await page.getByRole("button", { name: "Capture outside scope" }).click();
  const outsideResult = page.getByLabel("Outside scope result");
  await expect(outsideResult).toContainText(/^Event: [0-9a-f-]{36}$/);
  const outsideEventID = (await outsideResult.innerText()).match(/Event: ([0-9a-f-]{36})/)?.[1] ?? "";
  const outside = await waitForIssueEvent(request, "Outside completed trace scope", outsideEventID);
  expect(outside.event.contexts.trace).toBeUndefined();
});
