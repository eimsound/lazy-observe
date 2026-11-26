import {
  captureException,
  init,
  setPage,
  type BrowserClient,
  type BrowserConfig,
  type CaptureOptions,
  type ErrorEventPayload,
} from "@lazy-observe/browser";
import { registerLocalSignOutCleanup } from "@/lib/auth/session-expiry";
import { IS_MOCK } from "@/lib/settings";

export const SELF_MONITORING_CONFIG_PATH = "/api/system/self-monitoring";

const REPLAY_SESSION_STORAGE_KEY = "lazy-observe:session-id";
const REPLAY_SEQ_STORAGE_PREFIX = "lazy-observe:replay-seq:";

export interface WebSelfMonitoringConfig {
  enabled: true;
  projectSlug: string;
  token: string;
  serviceName: string;
  release?: string;
  replayEnabled: boolean;
}

export interface RouteMatchLike {
  routeId?: string;
  id?: string;
  fullPath?: string;
}

export interface SelfMonitoringBootstrapDeps {
  origin?: string;
  fetchImpl?: typeof fetch;
  isMock?: boolean;
  initClient?: (config: BrowserConfig) => BrowserClient;
}

export interface SelfMonitoringState {
  promise: Promise<BrowserClient | null> | null;
  client: BrowserClient | null;
  pendingPage: string | null;
  generation: number;
}

const defaultState: SelfMonitoringState = {
  promise: null,
  client: null,
  pendingPage: null,
  generation: 0,
};

const DYNAMIC_ROUTE_FALLBACKS: Array<[RegExp, string]> = [
  [/^\/traces\/[^/]+$/, "/traces/$traceID"],
  [/^\/issues\/[^/]+$/, "/issues/$issueId"],
  [/^\/replays\/[^/]+$/, "/replays/$sessionId"],
  [/^\/infrastructure\/hosts\/[^/]+$/, "/infrastructure/hosts/$instance"],
];

const NON_LEAF_ROUTE_IDS = new Set(["__root__", "app", "/app"]);

export function parseWebSelfMonitoringConfig(raw: unknown): WebSelfMonitoringConfig | null {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) {
    return null;
  }
  const obj = raw as Record<string, unknown>;
  if (obj.enabled !== true) {
    return null;
  }
  const projectSlug = typeof obj.project_slug === "string" ? obj.project_slug.trim() : "";
  const token = typeof obj.token === "string" ? obj.token.trim() : "";
  if (!projectSlug || !token.startsWith("lo_") || token.length <= 3) {
    return null;
  }
  const serviceNameRaw = typeof obj.service_name === "string" ? obj.service_name.trim() : "";
  const releaseRaw = typeof obj.release === "string" ? obj.release.trim() : "";
  return {
    enabled: true,
    projectSlug,
    token,
    serviceName: serviceNameRaw || "lazy-observe-web",
    ...(releaseRaw ? { release: releaseRaw } : {}),
    replayEnabled: obj.replay_enabled === true,
  };
}

export function buildSelfMonitoringDsn(origin: string, projectSlug: string, token: string): string {
  const base = origin.replace(/\/+$/, "");
  return `${base}/p/${encodeURIComponent(projectSlug)}@${token}`;
}

export function normalizeSelfMonitoringRoutePage(
  matches: ReadonlyArray<RouteMatchLike> | undefined,
  fallbackPathname: string,
): string {
  if (matches && matches.length > 0) {
    for (let i = matches.length - 1; i >= 0; i--) {
      const match = matches[i];
      const id = match.routeId ?? match.id ?? "";
      if (id === "__notFound__") {
        return "/_not-found";
      }
      if (NON_LEAF_ROUTE_IDS.has(id)) {
        continue;
      }
      const fullPath = match.fullPath?.trim();
      if (fullPath) {
        return fullPath;
      }
    }
  }
  const cleanPath = (fallbackPathname.split(/[?#]/, 1)[0] || "/").replace(/\/+$/, "") || "/";
  for (const [pattern, template] of DYNAMIC_ROUTE_FALLBACKS) {
    if (pattern.test(cleanPath)) {
      return template;
    }
  }
  return cleanPath;
}

export function filterSelfMonitoringError(event: ErrorEventPayload): ErrorEventPayload | null {
  if (event.exception?.type === "AbortError") {
    return null;
  }
  return event;
}

function buildBrowserConfig(origin: string, cfg: WebSelfMonitoringConfig): BrowserConfig {
  return {
    dsn: buildSelfMonitoringDsn(origin, cfg.projectSlug, cfg.token),
    serviceName: cfg.serviceName,
    ...(cfg.release ? { release: cfg.release } : {}),
    errors: true,
    vitals: true,
    fetchTracing: { autoPatch: true },
    replay: cfg.replayEnabled
      ? {
          onErrorOnly: true,
          onErrorFullCapture: true,
          blockSelector: "[data-lo-block]",
        }
      : false,
    beforeSend: filterSelfMonitoringError,
  };
}

function activateClient(
  origin: string,
  cfg: WebSelfMonitoringConfig,
  initClient: (config: BrowserConfig) => BrowserClient,
  state?: SelfMonitoringState,
): BrowserClient | null {
  try {
    const client = initClient(buildBrowserConfig(origin, cfg));
    if (state) {
      state.client = client;
      if (state.pendingPage) {
        client.setPage(state.pendingPage);
      }
    }
    return client;
  } catch {
    return null;
  }
}

type ConfigFetchResult =
  | { kind: "enabled"; config: WebSelfMonitoringConfig }
  | { kind: "disabled" }
  | { kind: "transient_error" };

async function fetchSelfMonitoringConfig(fetchImpl: typeof fetch): Promise<ConfigFetchResult> {
  try {
    const response = await fetchImpl(SELF_MONITORING_CONFIG_PATH, {
      credentials: "same-origin",
      cache: "no-store",
    });
    if (!response.ok) {
      return { kind: "transient_error" };
    }
    const payload: unknown = await response.json();
    const config = parseWebSelfMonitoringConfig(payload);
    return config ? { kind: "enabled", config } : { kind: "disabled" };
  } catch {
    return { kind: "transient_error" };
  }
}

export interface SessionStorageLike {
  readonly length: number;
  key(index: number): string | null;
  removeItem(key: string): void;
}

function defaultSessionStorage(): SessionStorageLike | null {
  try {
    return globalThis.sessionStorage ?? null;
  } catch {
    return null;
  }
}

export function clearSelfMonitoringSessionStorage(
  storage: SessionStorageLike | null = defaultSessionStorage(),
): void {
  if (!storage) return;
  try {
    const keysToRemove: string[] = [];
    for (let i = 0; i < storage.length; i++) {
      const key = storage.key(i);
      if (!key) continue;
      if (key === REPLAY_SESSION_STORAGE_KEY || key.startsWith(REPLAY_SEQ_STORAGE_PREFIX)) {
        keysToRemove.push(key);
      }
    }
    for (const key of keysToRemove) {
      storage.removeItem(key);
    }
  } catch {
    /* ignore unavailable storage */
  }
}

interface BootstrapOutcome {
  client: BrowserClient | null;
  retryable: boolean;
}

function bootstrapWithOutcome(
  deps: SelfMonitoringBootstrapDeps = {},
  state?: SelfMonitoringState,
): Promise<BootstrapOutcome> {
  const isMock = deps.isMock ?? IS_MOCK;
  if (isMock) {
    return Promise.resolve({ client: null, retryable: false });
  }
  const origin =
    deps.origin ?? (typeof window !== "undefined" ? window.location.origin : "");
  if (!origin) {
    return Promise.resolve({ client: null, retryable: false });
  }
  const initClient = deps.initClient ?? init;
  const fetchImpl =
    deps.fetchImpl ??
    (typeof window !== "undefined" && typeof window.fetch === "function"
      ? window.fetch.bind(window)
      : undefined);
  if (!fetchImpl) {
    return Promise.resolve({ client: null, retryable: false });
  }
  const startGeneration = state?.generation ?? 0;
  return fetchSelfMonitoringConfig(fetchImpl).then((result) => {
    if (state && state.generation !== startGeneration) {
      return { client: null, retryable: false };
    }
    if (result.kind === "transient_error") {
      return { client: null, retryable: true };
    }
    if (result.kind === "disabled") {
      return { client: null, retryable: false };
    }
    return {
      client: activateClient(origin, result.config, initClient, state),
      retryable: false,
    };
  });
}

export function bootstrapWebSelfMonitoring(
  deps: SelfMonitoringBootstrapDeps = {},
  state?: SelfMonitoringState,
): Promise<BrowserClient | null> {
  return bootstrapWithOutcome(deps, state).then((outcome) => outcome.client);
}

export function initWebSelfMonitoring(
  deps: SelfMonitoringBootstrapDeps = {},
  state: SelfMonitoringState = defaultState,
): Promise<BrowserClient | null> {
  if (deps.origin === undefined && typeof window === "undefined") {
    return Promise.resolve(null);
  }
  if (!state.promise) {
    const startGeneration = state.generation;
    const pending = bootstrapWithOutcome(deps, state).then((outcome) => {
      if (outcome.retryable && state.generation === startGeneration && state.promise === pending) {
        state.promise = null;
      }
      return outcome.client;
    });
    state.promise = pending;
  }
  return state.promise;
}

export function teardownWebSelfMonitoring(
  state: SelfMonitoringState = defaultState,
  storage: SessionStorageLike | null = defaultSessionStorage(),
): void {
  state.generation += 1;
  const activeClient = state.client;
  state.client = null;
  state.promise = null;
  state.pendingPage = null;
  if (activeClient) {
    try {
      void activeClient.close();
    } catch {
      /* keep sign-out moving even if closing telemetry fails */
    }
  }
  clearSelfMonitoringSessionStorage(storage);
}

registerLocalSignOutCleanup(() => {
  teardownWebSelfMonitoring();
});

export function trackSelfMonitoringRoute(routePage: string): void {
  defaultState.pendingPage = routePage;
  try {
    if (defaultState.client) {
      defaultState.client.setPage(routePage);
    } else {
      setPage(routePage);
    }
  } catch {
    /* ignore */
  }
}

export function captureSelfMonitoringException(error: unknown, options?: CaptureOptions): void {
  try {
    if (defaultState.client) {
      defaultState.client.captureException(error, options);
    } else {
      captureException(error, options);
    }
  } catch {
    /* ignore */
  }
}
