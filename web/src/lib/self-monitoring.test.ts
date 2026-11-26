import assert from "node:assert/strict";
import { test } from "vitest";
import type { BrowserClient, BrowserConfig, ErrorEventPayload } from "@lazy-observe/browser";
import {
  bootstrapWebSelfMonitoring,
  buildSelfMonitoringDsn,
  clearSelfMonitoringSessionStorage,
  filterSelfMonitoringError,
  initWebSelfMonitoring,
  normalizeSelfMonitoringRoutePage,
  parseWebSelfMonitoringConfig,
  SELF_MONITORING_CONFIG_PATH,
  teardownWebSelfMonitoring,
  type SelfMonitoringState,
  type SessionStorageLike,
} from "./self-monitoring";

function fakeClient(setPages: string[], closedOut?: { value: number }): BrowserClient {
  return {
    sessionId: "sess-1",
    startSpan: (_opts, cb) => cb({ name: "span", context: { trace_id: "0".repeat(32), span_id: "0".repeat(16) } }),
    captureException: () => "evt-1",
    wrapFetch: (impl) => impl as typeof fetch,
    setPage: (page) => {
      setPages.push(page);
    },
    setCountry: () => undefined,
    diagnostics: () => ({ droppedTelemetry: 0 }),
    flush: async () => undefined,
    close: async () => {
      if (closedOut) closedOut.value += 1;
    },
  };
}

class MemoryStorage implements SessionStorageLike {
  readonly data = new Map<string, string>();
  constructor(initial: Record<string, string> = {}) {
    for (const [k, v] of Object.entries(initial)) {
      this.data.set(k, v);
    }
  }
  get length(): number {
    return this.data.size;
  }
  key(index: number): string | null {
    return Array.from(this.data.keys())[index] ?? null;
  }
  removeItem(key: string): void {
    this.data.delete(key);
  }
}

test("parseWebSelfMonitoringConfig defaults replayEnabled to false and fails closed on invalid payloads", () => {
  assert.deepEqual(
    parseWebSelfMonitoringConfig({
      enabled: true,
      project_slug: "lazy-observe",
      token: "lo_web_self_token_123",
      service_name: "lazy-observe-web",
      release: "v1.2.3",
    }),
    {
      enabled: true,
      projectSlug: "lazy-observe",
      token: "lo_web_self_token_123",
      serviceName: "lazy-observe-web",
      release: "v1.2.3",
      replayEnabled: false,
    },
  );
  assert.deepEqual(
    parseWebSelfMonitoringConfig({
      enabled: true,
      project_slug: "lazy-observe",
      token: "lo_web_self_token_123",
      replay_enabled: true,
    }),
    {
      enabled: true,
      projectSlug: "lazy-observe",
      token: "lo_web_self_token_123",
      serviceName: "lazy-observe-web",
      replayEnabled: true,
    },
  );

  assert.equal(parseWebSelfMonitoringConfig(null), null);
  assert.equal(parseWebSelfMonitoringConfig({ enabled: false }), null);
  assert.equal(parseWebSelfMonitoringConfig({ enabled: true, project_slug: "lazy-observe", token: "" }), null);
  assert.equal(
    parseWebSelfMonitoringConfig({ enabled: true, project_slug: "lazy-observe", token: "lou_user_token" }),
    null,
  );
  assert.equal(
    parseWebSelfMonitoringConfig({ enabled: true, project_slug: "", token: "lo_web_self_token_123" }),
    null,
  );
});

test("buildSelfMonitoringDsn encodes project slug and preserves token", () => {
  assert.equal(
    buildSelfMonitoringDsn("https://observe.example.com/", "lazy-observe", "lo_abc123"),
    "https://observe.example.com/p/lazy-observe@lo_abc123",
  );
});

test("normalizeSelfMonitoringRoutePage maps dynamic routes to low-cardinality templates", () => {
  assert.equal(
    normalizeSelfMonitoringRoutePage(
      [
        { routeId: "__root__", fullPath: "/" },
        { routeId: "app", fullPath: "/" },
        { routeId: "/app/traces/$traceID", fullPath: "/traces/$traceID" },
      ],
      "/traces/0123456789abcdef",
    ),
    "/traces/$traceID",
  );
  assert.equal(
    normalizeSelfMonitoringRoutePage([{ routeId: "__root__", fullPath: "/" }, { routeId: "__notFound__" }], "/nope"),
    "/_not-found",
  );
  assert.equal(normalizeSelfMonitoringRoutePage([], "/issues/42?event=abc"), "/issues/$issueId");
  assert.equal(normalizeSelfMonitoringRoutePage([], "/replays/sess_123"), "/replays/$sessionId");
  assert.equal(normalizeSelfMonitoringRoutePage([], "/infrastructure/hosts/node-1"), "/infrastructure/hosts/$instance");
  assert.equal(normalizeSelfMonitoringRoutePage([], "/logs"), "/logs");
});

test("bootstrapWebSelfMonitoring initializes via session endpoint with replay disabled by default", async () => {
  const requestedURLs: string[] = [];
  const requestedInit: RequestInit[] = [];
  const configs: BrowserConfig[] = [];
  const pages: string[] = [];
  const state: SelfMonitoringState = {
    promise: null,
    client: null,
    pendingPage: "/logs",
    generation: 0,
  };

  const client = await bootstrapWebSelfMonitoring(
    {
      origin: "http://127.0.0.1:3100",
      isMock: false,
      fetchImpl: (async (input: RequestInfo | URL, init?: RequestInit) => {
        requestedURLs.push(String(input));
        if (init) requestedInit.push(init);
        return new Response(
          JSON.stringify({
            enabled: true,
            project_slug: "lazy-observe",
            token: "lo_session_web_token",
            service_name: "lazy-observe-web",
            release: "v0.2.0",
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        );
      }) as typeof fetch,
      initClient: (cfg) => {
        configs.push(cfg);
        return fakeClient(pages);
      },
    },
    state,
  );

  assert.ok(client);
  assert.deepEqual(requestedURLs, [SELF_MONITORING_CONFIG_PATH]);
  assert.equal(requestedInit[0]?.credentials, "same-origin");
  assert.equal(requestedInit[0]?.cache, "no-store");
  assert.equal(configs.length, 1);
  assert.equal(configs[0].dsn, "http://127.0.0.1:3100/p/lazy-observe@lo_session_web_token");
  assert.equal(configs[0].serviceName, "lazy-observe-web");
  assert.equal(configs[0].release, "v0.2.0");
  assert.equal(configs[0].errors, true);
  assert.equal(configs[0].vitals, true);
  assert.deepEqual(configs[0].fetchTracing, { autoPatch: true });
  assert.equal(configs[0].replay, false);
  assert.deepEqual(pages, ["/logs"]);
});

test("bootstrapWebSelfMonitoring enables onErrorOnly replay only when replay_enabled is true", async () => {
  const configs: BrowserConfig[] = [];
  const client = await bootstrapWebSelfMonitoring({
    origin: "https://observe.example.com",
    isMock: false,
    fetchImpl: (async () =>
      new Response(
        JSON.stringify({
          enabled: true,
          project_slug: "lazy-observe",
          token: "lo_replay_web_token",
          replay_enabled: true,
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      )) as typeof fetch,
    initClient: (cfg) => {
      configs.push(cfg);
      return fakeClient([]);
    },
  });

  assert.ok(client);
  assert.deepEqual(configs[0]?.replay, {
    onErrorOnly: true,
    onErrorFullCapture: true,
    blockSelector: "[data-lo-block]",
  });
});

test("bootstrapWebSelfMonitoring skips initialization in mock mode, 401/network failure, or disabled response", async () => {
  let fetchCalled = false;
  const mockClient = await bootstrapWebSelfMonitoring({
    origin: "http://127.0.0.1:3100",
    isMock: true,
    fetchImpl: (async () => {
      fetchCalled = true;
      return new Response("{}", { status: 200 });
    }) as typeof fetch,
  });
  assert.equal(mockClient, null);
  assert.equal(fetchCalled, false);

  const unauthClient = await bootstrapWebSelfMonitoring({
    origin: "http://127.0.0.1:3100",
    isMock: false,
    fetchImpl: (async () => new Response("{}", { status: 401 })) as typeof fetch,
  });
  assert.equal(unauthClient, null);

  const disabledClient = await bootstrapWebSelfMonitoring({
    origin: "http://127.0.0.1:3100",
    isMock: false,
    fetchImpl: (async () => new Response(JSON.stringify({ enabled: false }), { status: 200 })) as typeof fetch,
  });
  assert.equal(disabledClient, null);

  const failedClient = await bootstrapWebSelfMonitoring({
    origin: "http://127.0.0.1:3100",
    isMock: false,
    fetchImpl: (async () => {
      throw new Error("network down");
    }) as typeof fetch,
  });
  assert.equal(failedClient, null);
});

test("initWebSelfMonitoring retries after transient failures, caches explicit disabled, and guards generation races", async () => {
  // 1) Transient 503 clears state.promise so a subsequent call retries and activates.
  let calls503 = 0;
  const state503: SelfMonitoringState = {
    promise: null,
    client: null,
    pendingPage: "/logs",
    generation: 0,
  };
  const deps503 = {
    origin: "http://127.0.0.1:3100",
    isMock: false,
    fetchImpl: (async () => {
      calls503 += 1;
      if (calls503 === 1) {
        return new Response("unavailable", { status: 503 });
      }
      return new Response(
        JSON.stringify({
          enabled: true,
          project_slug: "lazy-observe",
          token: "lo_retry_after_503",
        }),
        { status: 200 },
      );
    }) as typeof fetch,
    initClient: () => fakeClient([]),
  };
  assert.equal(await initWebSelfMonitoring(deps503, state503), null);
  assert.equal(state503.promise, null);
  const recovered503 = await initWebSelfMonitoring(deps503, state503);
  assert.ok(recovered503);
  assert.equal(calls503, 2);

  // 2) Network rejection also clears state.promise so a subsequent call retries.
  let callsNet = 0;
  const stateNet: SelfMonitoringState = {
    promise: null,
    client: null,
    pendingPage: null,
    generation: 0,
  };
  const depsNet = {
    origin: "http://127.0.0.1:3100",
    isMock: false,
    fetchImpl: (async () => {
      callsNet += 1;
      if (callsNet === 1) {
        throw new Error("temporary network error");
      }
      return new Response(
        JSON.stringify({
          enabled: true,
          project_slug: "lazy-observe",
          token: "lo_retry_after_network",
        }),
        { status: 200 },
      );
    }) as typeof fetch,
    initClient: () => fakeClient([]),
  };
  assert.equal(await initWebSelfMonitoring(depsNet, stateNet), null);
  assert.equal(stateNet.promise, null);
  const recoveredNet = await initWebSelfMonitoring(depsNet, stateNet);
  assert.ok(recoveredNet);
  assert.equal(callsNet, 2);

  // 3) Explicit { enabled: false } stays settled/cached without re-fetching.
  let callsDisabled = 0;
  const stateDisabled: SelfMonitoringState = {
    promise: null,
    client: null,
    pendingPage: null,
    generation: 0,
  };
  const depsDisabled = {
    origin: "http://127.0.0.1:3100",
    isMock: false,
    fetchImpl: (async () => {
      callsDisabled += 1;
      return new Response(JSON.stringify({ enabled: false }), { status: 200 });
    }) as typeof fetch,
    initClient: () => fakeClient([]),
  };
  assert.equal(await initWebSelfMonitoring(depsDisabled, stateDisabled), null);
  assert.notEqual(stateDisabled.promise, null);
  assert.equal(await initWebSelfMonitoring(depsDisabled, stateDisabled), null);
  assert.equal(callsDisabled, 1);

  // 4) Stale transient failure from an earlier generation must not clear the newer generation's promise.
  const stateRace: SelfMonitoringState = {
    promise: null,
    client: null,
    pendingPage: null,
    generation: 0,
  };
  let resolveFirst!: (resp: Response) => void;
  const firstCall = initWebSelfMonitoring(
    {
      origin: "http://127.0.0.1:3100",
      isMock: false,
      fetchImpl: (() =>
        new Promise<Response>((resolve) => {
          resolveFirst = resolve;
        })) as typeof fetch,
      initClient: () => fakeClient([]),
    },
    stateRace,
  );
  teardownWebSelfMonitoring(stateRace, new MemoryStorage());
  let resolveSecond!: (resp: Response) => void;
  const secondCall = initWebSelfMonitoring(
    {
      origin: "http://127.0.0.1:3100",
      isMock: false,
      fetchImpl: (() =>
        new Promise<Response>((resolve) => {
          resolveSecond = resolve;
        })) as typeof fetch,
      initClient: () => fakeClient([]),
    },
    stateRace,
  );
  const secondPromiseRef = stateRace.promise;
  resolveFirst(new Response("unavailable", { status: 503 }));
  assert.equal(await firstCall, null);
  assert.equal(stateRace.promise, secondPromiseRef);
  resolveSecond(
    new Response(
      JSON.stringify({ enabled: true, project_slug: "lazy-observe", token: "lo_gen2_token" }),
      { status: 200 },
    ),
  );
  assert.ok(await secondCall);
});

test("teardownWebSelfMonitoring closes client, cancels in-flight bootstrap, and clears SDK sessionStorage keys", async () => {
  const closedCount = { value: 0 };
  const state: SelfMonitoringState = {
    promise: Promise.resolve(null),
    client: fakeClient([], closedCount),
    pendingPage: "/issues",
    generation: 0,
  };
  const storage = new MemoryStorage({
    "lazy-observe:session-id": "sess-1",
    "lazy-observe:replay-seq:sess-1": "4",
    "other-key": "keep-me",
  });

  teardownWebSelfMonitoring(state, storage);
  assert.equal(closedCount.value, 1);
  assert.equal(state.client, null);
  assert.equal(state.promise, null);
  assert.equal(state.pendingPage, null);
  assert.equal(state.generation, 1);
  assert.deepEqual(Array.from(storage.data.keys()), ["other-key"]);

  // In-flight bootstrap interrupted by sign-out must not activate the client afterwards.
  let resolveFetch!: (resp: Response) => void;
  let initCalled = false;
  const pendingBootstrap = bootstrapWebSelfMonitoring(
    {
      origin: "http://127.0.0.1:3100",
      isMock: false,
      fetchImpl: (() =>
        new Promise<Response>((resolve) => {
          resolveFetch = resolve;
        })) as typeof fetch,
      initClient: () => {
        initCalled = true;
        return fakeClient([]);
      },
    },
    state,
  );
  teardownWebSelfMonitoring(state, storage);
  resolveFetch(
    new Response(
      JSON.stringify({ enabled: true, project_slug: "lazy-observe", token: "lo_late_token" }),
      { status: 200 },
    ),
  );
  const lateClient = await pendingBootstrap;
  assert.equal(lateClient, null);
  assert.equal(initCalled, false);

  clearSelfMonitoringSessionStorage(null);
});

test("filterSelfMonitoringError drops AbortError and forwards real errors", () => {
  const baseEvent: ErrorEventPayload = {
    event_id: "evt-1",
    timestamp: "2026-11-26T00:00:00.000Z",
    platform: "javascript",
    level: "error",
    exception: {
      type: "AbortError",
      value: "The operation was aborted.",
      stacktrace: { frames: [] },
    },
  };
  assert.equal(filterSelfMonitoringError(baseEvent), null);

  const realError: ErrorEventPayload = {
    ...baseEvent,
    exception: { ...baseEvent.exception, type: "TypeError", value: "Cannot read properties" },
  };
  assert.equal(filterSelfMonitoringError(realError), realError);
});
