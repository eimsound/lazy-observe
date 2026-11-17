import type {
  LogRow,
  LogsHitsResponse,
  LogsQueryTimeRange,
  LogsStatsQueryRangeResponse,
} from "../lib/api/logs.ts";
import {
  DEFAULT_LOGS_LIMIT,
  DEFAULT_LOGS_QUERY,
  DEFAULT_LOGS_RANGE_SECONDS,
  MAX_LOGS_LIMIT,
  logsRangeFromSearch,
  logsRangeToSearch,
  parseLogsFilters,
  serializeLogsFilters,
  type LogsExtraFilter,
  type LogsHistoryEntry,
} from "../lib/logs-explorer.ts";
import {
  formatPrometheusDuration,
  parsePrometheusDuration,
  resolveMetricsRange,
  type MetricsRange,
} from "../lib/metrics-query.ts";

export type LogsExplorationMode = "historical" | "live";
export type LogsHistoricalView = "group" | "table" | "json";
export type LogsFiniteSlotName = "timeRange" | "rows" | "hits" | "stats";

export interface LogsExplorationIntent {
  project: string;
  draft: string;
  range: MetricsRange;
  filters: LogsExtraFilter[];
  limit: number;
  refreshSeconds: number;
  mode: LogsExplorationMode;
  historicalView: LogsHistoricalView;
  liveOffset: string;
  liveLimit: number;
}

export interface ExecutedLogsQuery {
  executionId: number;
  project: string;
  query: string;
  range: MetricsRange;
  start: number;
  end: number;
  limit: number;
  filters: LogsExtraFilter[];
}

export interface LogsResultSlot<T> {
  status: "idle" | "loading" | "success" | "error";
  identity: string | null;
  data: T | null;
  error: string | null;
}

export interface LogsFiniteResults {
  timeRange: LogsResultSlot<LogsQueryTimeRange>;
  rows: LogsResultSlot<LogRow[]>;
  hits: LogsResultSlot<LogsHitsResponse>;
  stats: LogsResultSlot<LogsStatsQueryRangeResponse>;
}

export interface LogsLiveSnapshot {
  status: "idle" | "connecting" | "open" | "retrying" | "stopped" | "error";
  identity: number | null;
  rows: LogRow[];
  dropped: number;
  rate: number;
  error: string | null;
  retryAttempt: number;
}

export interface LogsExplorationSnapshot {
  intent: LogsExplorationIntent;
  executed: ExecutedLogsQuery | null;
  effectiveRange: { start: number; end: number } | null;
  finite: LogsFiniteResults;
  live: LogsLiveSnapshot;
  history: LogsHistoryEntry[];
}

export interface LogsAuxiliaryQueryConfig {
  rowsEnabled: boolean;
  chartEnabled: boolean;
  graphMode: "hits" | "stats";
  groupBy: string;
  topN: number;
  step: number | null;
}

interface LogsFinitePlanBase {
  executionId: number;
  identity: string;
  snapshot: ExecutedLogsQuery;
}

export type LogsFiniteQueryPlan = LogsFinitePlanBase & (
  | { slot: "timeRange" }
  | {
    slot: "rows";
    range: { start: number; end: number };
  }
  | {
    slot: "hits";
    range: { start: number; end: number };
    step: number;
    groupBy?: string;
    topN: number;
  }
  | {
    slot: "stats";
    range: { start: number; end: number };
    step: number;
    topN: number;
  }
);

export interface LogsLiveRequest {
  identity: number;
  project: string;
  query: string;
  offset: string;
  filters: LogsExtraFilter[];
}

export interface LogsLiveObserver {
  opened: () => void;
  row: (row: LogRow) => void;
  failed: (failure: LogsLiveFailure) => void;
}

export interface LogsLiveFailure {
  message: string;
  status?: number;
  kind?: "network" | "timeout" | "query" | "abort" | "unknown";
}

export interface LogsLiveConnection {
  close: () => void;
}

export interface LogsWorkspaceClock {
  now: () => number;
  random: () => number;
  setTimeout: (callback: () => void, delay: number) => number;
  clearTimeout: (id: number) => void;
  setInterval: (callback: () => void, delay: number) => number;
  clearInterval: (id: number) => void;
}

export interface LogsHistoryAdapter {
  load: (project: string) => LogsHistoryEntry[];
  push: (project: string, query: string, now: number) => LogsHistoryEntry[];
  toggleFavorite: (project: string, query: string) => LogsHistoryEntry[];
  remove: (project: string, query: string) => LogsHistoryEntry[];
  clear: (project: string) => LogsHistoryEntry[];
}

export interface CreateLogsExplorationWorkspaceOptions {
  project: string;
  location: string;
  clock: LogsWorkspaceClock;
  history: LogsHistoryAdapter;
  deferInitialExecution?: boolean;
  initialAuxiliaryConfig?: LogsAuxiliaryQueryConfig;
  onLocationReplace: (location: string) => void;
  onFiniteQueryPlan: (plan: LogsFiniteQueryPlan) => void;
  onFiniteQueryCancel: (executionId: number) => void;
  openLiveStream: (
    request: LogsLiveRequest,
    observer: LogsLiveObserver,
  ) => LogsLiveConnection;
}

export type LogsExplorationEvent =
  | { type: "activated" }
  | { type: "draftChanged"; draft: string }
  | { type: "rangeChanged"; range: MetricsRange }
  | { type: "filtersChanged"; filters: LogsExtraFilter[] }
  | { type: "limitChanged"; limit: number }
  | { type: "refreshSecondsChanged"; refreshSeconds: number }
  | {
    type: "viewChanged";
    view: LogsHistoricalView | "live";
  }
  | { type: "liveOffsetChanged"; offset: string }
  | { type: "liveLimitChanged"; limit: number }
  | { type: "configureAuxiliaryQueries"; config: LogsAuxiliaryQueryConfig }
  | { type: "run" }
  | { type: "refresh" }
  | { type: "retry"; slot: LogsFiniteSlotName }
  | { type: "cancelFinite" }
  | { type: "finiteSucceeded"; identity: string; data: unknown }
  | { type: "finiteFailed"; identity: string; error: string }
  | { type: "projectChanged"; project: string }
  | { type: "externalLocationChanged"; location: string }
  | { type: "reset" }
  | { type: "historyFavoriteToggled"; query: string }
  | { type: "historyRemoved"; query: string }
  | { type: "historyCleared" }
  | { type: "liveStopped" }
  | { type: "liveResumed" }
  | { type: "liveRetried" }
  | { type: "liveCleared" };

export interface LogsExplorationWorkspace {
  getSnapshot: () => LogsExplorationSnapshot;
  subscribe: (listener: () => void) => () => void;
  send: (event: LogsExplorationEvent) => void;
  dispose: () => void;
}

interface MutableWorkspaceState {
  intent: LogsExplorationIntent;
  executed: ExecutedLogsQuery | null;
  effectiveRange: { start: number; end: number } | null;
  finite: LogsFiniteResults;
  live: LogsLiveSnapshot;
  history: LogsHistoryEntry[];
}

const DEFAULT_AUXILIARY_CONFIG: LogsAuxiliaryQueryConfig = {
  rowsEnabled: true,
  chartEnabled: false,
  graphMode: "hits",
  groupBy: "",
  topN: 5,
  step: null,
};

function idleSlot<T>(): LogsResultSlot<T> {
  return { status: "idle", identity: null, data: null, error: null };
}

function idleFiniteResults(): LogsFiniteResults {
  return {
    timeRange: idleSlot(),
    rows: idleSlot(),
    hits: idleSlot(),
    stats: idleSlot(),
  };
}

function idleLive(): LogsLiveSnapshot {
  return {
    status: "idle",
    identity: null,
    rows: [],
    dropped: 0,
    rate: 0,
    error: null,
    retryAttempt: 0,
  };
}

function boundedInteger(
  value: string | null,
  fallback: number,
  minimum: number,
  maximum: number,
): number {
  if (value === null || !value.trim()) return fallback;
  const parsed = Number(value);
  return Number.isFinite(parsed)
    ? Math.max(minimum, Math.min(maximum, Math.floor(parsed)))
    : fallback;
}

function normalizedLiveOffset(value: string | null): string {
  const seconds = boundedInteger(value, 5, 1, 3_600);
  return `${seconds}s`;
}

function relativeLocation(value: string): string {
  const url = new URL(value, "https://lazy-observe.invalid");
  return `${url.pathname}${url.search}${url.hash}`;
}

function parseIntent(project: string, location: string): LogsExplorationIntent {
  const url = new URL(location, "https://lazy-observe.invalid");
  const params = url.searchParams;
  const draft = params.get("q")?.trim() || DEFAULT_LOGS_QUERY;
  const rawView = params.get("view");
  const historicalView: LogsHistoricalView = rawView === "table" || rawView === "json"
    ? rawView
    : "group";
  return {
    project,
    draft,
    range: logsRangeFromSearch({
      range: params.get("range") ?? undefined,
      start: params.get("start") ?? undefined,
      end: params.get("end") ?? undefined,
    }),
    filters: parseLogsFilters(params.get("filters")),
    limit: boundedInteger(params.get("limit"), DEFAULT_LOGS_LIMIT, 1, MAX_LOGS_LIMIT),
    refreshSeconds: boundedInteger(params.get("refresh"), 0, 0, 3_600),
    mode: rawView === "live" ? "live" : "historical",
    historicalView,
    liveOffset: normalizedLiveOffset(params.get("live_tailing_offset")),
    liveLimit: boundedInteger(params.get("rows_per_page"), 100, 10, 50_000),
  };
}

function locationRequestsExecution(location: string): boolean {
  const value = new URL(location, "https://lazy-observe.invalid").searchParams.get("run");
  return value === "1" || value === "true";
}

function hasQueryTimeFilter(query: string): boolean {
  return /(?:^|[\s(])_time\s*:/.test(query);
}

function liveRowFingerprint(row: LogRow): string {
  const stream = row.raw._stream_id ?? row.raw._stream ?? row.stream;
  const timestamp = row.raw._time ?? row.timeRaw ?? row.time;
  return `${String(stream)}\u0000${String(timestamp)}\u0000${JSON.stringify(row.raw)}`;
}

function retryableLiveFailure(failure: LogsLiveFailure): boolean {
  if (failure.kind === "abort" || failure.kind === "query") return false;
  if (failure.status === 401 || failure.status === 403) return false;
  if (failure.status !== undefined) {
    return failure.status === 429 || failure.status >= 500;
  }
  return failure.kind === "network" || failure.kind === "timeout";
}

function timestampSeconds(value: unknown): number | null {
  if (typeof value === "number" && Number.isFinite(value)) {
    const absolute = Math.abs(value);
    return absolute >= 1e15 ? value / 1e9 : absolute >= 1e11 ? value / 1e3 : value;
  }
  if (typeof value !== "string" || !value.trim()) return null;
  const numeric = Number(value);
  if (Number.isFinite(numeric)) return timestampSeconds(numeric);
  const milliseconds = Date.parse(value);
  return Number.isFinite(milliseconds) ? milliseconds / 1_000 : null;
}

function cloneIntent(intent: LogsExplorationIntent): LogsExplorationIntent {
  return {
    ...intent,
    range: { ...intent.range },
    filters: intent.filters.map((filter) => ({ ...filter })),
  };
}

function snapshotOf(state: MutableWorkspaceState): LogsExplorationSnapshot {
  return {
    intent: cloneIntent(state.intent),
    executed: state.executed
      ? {
        ...state.executed,
        range: { ...state.executed.range },
        filters: state.executed.filters.map((filter) => ({ ...filter })),
      }
      : null,
    effectiveRange: state.effectiveRange ? { ...state.effectiveRange } : null,
    finite: {
      timeRange: { ...state.finite.timeRange },
      rows: { ...state.finite.rows },
      hits: { ...state.finite.hits },
      stats: { ...state.finite.stats },
    },
    live: { ...state.live, rows: [...state.live.rows] },
    history: state.history.map((entry) => ({ ...entry })),
  };
}

export function createLogsExplorationWorkspace(
  options: CreateLogsExplorationWorkspaceOptions,
): LogsExplorationWorkspace {
  let currentLocation = relativeLocation(options.location);
  let auxiliaryConfig = {
    ...DEFAULT_AUXILIARY_CONFIG,
    ...options.initialAuxiliaryConfig,
  };
  let nextExecutionId = 1;
  let nextPlanAttempt = 1;
  let nextLiveIdentity = 1;
  let nextLiveRowIndex = 1;
  let disposed = false;
  let refreshTimer: number | null = null;
  let liveConnection: LogsLiveConnection | null = null;
  let liveRetryTimer: number | null = null;
  let liveFlushTimer: number | null = null;
  let pendingLiveRows: LogRow[] = [];
  let pendingLiveRate = 0;
  const liveSeen = new Set<string>();
  const listeners = new Set<() => void>();

  const state: MutableWorkspaceState = {
    intent: parseIntent(options.project, currentLocation),
    executed: null,
    effectiveRange: null,
    finite: idleFiniteResults(),
    live: idleLive(),
    history: options.history.load(options.project),
  };
  let publicSnapshot = snapshotOf(state);

  const publish = () => {
    if (disposed) return;
    publicSnapshot = snapshotOf(state);
    listeners.forEach((listener) => listener());
  };

  const canonicalLocation = (run: boolean): string => {
    const url = new URL(currentLocation, "https://lazy-observe.invalid");
    const params = url.searchParams;
    const setOptional = (key: string, value: string | number | undefined) => {
      if (value === undefined || value === "") params.delete(key);
      else params.set(key, String(value));
    };
    const range = logsRangeToSearch(state.intent.range);
    const draft = state.intent.draft || DEFAULT_LOGS_QUERY;
    const defaultRange = state.intent.range.mode === "relative"
      && state.intent.range.seconds === DEFAULT_LOGS_RANGE_SECONDS;
    setOptional("q", draft === DEFAULT_LOGS_QUERY ? undefined : draft);
    setOptional("run", run ? "1" : undefined);
    setOptional("range", defaultRange ? undefined : range.range);
    setOptional("start", range.start);
    setOptional("end", range.end);
    setOptional("limit", state.intent.limit === DEFAULT_LOGS_LIMIT
      ? undefined
      : state.intent.limit);
    setOptional("filters", serializeLogsFilters(state.intent.filters));
    setOptional("refresh", state.intent.refreshSeconds || undefined);
    setOptional("view", state.intent.mode === "live"
      ? "live"
      : state.intent.historicalView === "group"
        ? undefined
        : state.intent.historicalView);
    const offsetSeconds = parsePrometheusDuration(state.intent.liveOffset);
    setOptional("live_tailing_offset", offsetSeconds === 5 ? undefined : offsetSeconds ?? 5);
    if (state.intent.mode === "live") {
      setOptional("rows_per_page", state.intent.liveLimit === 100
        ? undefined
        : state.intent.liveLimit);
    }
    return `${url.pathname}?${params.toString()}${url.hash}`.replace("?#", "#");
  };

  const replaceLocation = (run: boolean) => {
    const executedMatchesIntent = Boolean(
      state.executed
      && state.executed.query === state.intent.draft.trim()
      && state.executed.limit === state.intent.limit
      && JSON.stringify(state.executed.range) === JSON.stringify(state.intent.range)
      && serializeLogsFilters(state.executed.filters) === serializeLogsFilters(state.intent.filters),
    );
    const target = canonicalLocation(run && (!state.executed || executedMatchesIntent));
    if (target === currentLocation) return;
    currentLocation = target;
    options.onLocationReplace(target);
  };

  const clearRefreshTimer = () => {
    if (refreshTimer === null) return;
    options.clock.clearInterval(refreshTimer);
    refreshTimer = null;
  };

  const stopLiveResources = () => {
    liveConnection?.close();
    liveConnection = null;
    if (liveRetryTimer !== null) options.clock.clearTimeout(liveRetryTimer);
    if (liveFlushTimer !== null) options.clock.clearInterval(liveFlushTimer);
    liveRetryTimer = null;
    liveFlushTimer = null;
    pendingLiveRows = [];
    pendingLiveRate = 0;
  };

  const cancelFinite = () => {
    if (!state.executed) return;
    const loading = Object.values(state.finite).some((slot) => slot.status === "loading");
    if (loading) options.onFiniteQueryCancel(state.executed.executionId);
    for (const slotName of Object.keys(state.finite) as LogsFiniteSlotName[]) {
      const slot = state.finite[slotName];
      if (slot.status === "loading") state.finite[slotName] = idleSlot() as never;
    }
  };

  const planIdentity = (executionId: number, slot: LogsFiniteSlotName) => (
    `${executionId}:${slot}:${nextPlanAttempt++}`
  );

  const emitPlan = (
    slot: LogsFiniteSlotName,
    plan: Omit<LogsFiniteQueryPlan, keyof LogsFinitePlanBase | "slot"> = {},
    preserveData = false,
  ) => {
    const executed = state.executed;
    if (!executed) return;
    const identity = planIdentity(executed.executionId, slot);
    state.finite[slot] = {
      status: "loading",
      identity,
      data: preserveData ? state.finite[slot].data : null,
      error: null,
    } as never;
    options.onFiniteQueryPlan({
      executionId: executed.executionId,
      identity,
      snapshot: { ...executed, range: { ...executed.range }, filters: [...executed.filters] },
      slot,
      ...plan,
    } as LogsFiniteQueryPlan);
  };

  const emitResolvedPlans = (preserveData = false) => {
    const range = state.effectiveRange;
    if (!range) return;
    if (auxiliaryConfig.rowsEnabled) emitPlan("rows", { range }, preserveData);
    if (auxiliaryConfig.chartEnabled) emitChartPlan(auxiliaryConfig.graphMode, preserveData);
  };

  const emitChartPlan = (slot: "hits" | "stats", preserveData = false) => {
    const range = state.effectiveRange;
    if (!range) return;
    const seconds = Math.max(1, range.end - range.start);
    const plan = {
      range,
      step: auxiliaryConfig.step ?? Math.max(1, Math.ceil(seconds / 300)),
      topN: auxiliaryConfig.topN,
    };
    if (slot === "hits") {
      emitPlan("hits", { ...plan, groupBy: auxiliaryConfig.groupBy || undefined }, preserveData);
    } else {
      emitPlan("stats", plan, preserveData);
    }
  };

  const startHistoricalExecution = (
    source: "run" | "refresh" | "retry" | "project" | "location" | "mode" | "intent",
    snapshot = state.executed,
  ) => {
    const previousExecutedQuery = state.executed?.query;
    const query = source === "run" || source === "mode"
      ? state.intent.draft.trim()
      : snapshot?.query ?? state.intent.draft.trim();
    if (!query || state.intent.limit < 1 || state.intent.limit > MAX_LOGS_LIMIT) return;
    const reuseExecutedSnapshot = source === "refresh" || source === "retry";
    const retainedFinite = reuseExecutedSnapshot
      ? {
        timeRange: { ...state.finite.timeRange },
        rows: { ...state.finite.rows },
        hits: { ...state.finite.hits },
        stats: { ...state.finite.stats },
      }
      : null;
    cancelFinite();
    stopLiveResources();
    const executionRange = reuseExecutedSnapshot && snapshot
      ? snapshot.range
      : state.intent.range;
    const resolved = resolveMetricsRange(executionRange, options.clock.now());
    const executionId = nextExecutionId++;
    state.executed = {
      executionId,
      project: state.intent.project,
      query,
      range: { ...executionRange },
      start: resolved.start,
      end: resolved.end,
      limit: reuseExecutedSnapshot && snapshot ? snapshot.limit : state.intent.limit,
      filters: (reuseExecutedSnapshot && snapshot ? snapshot.filters : state.intent.filters)
        .map((filter) => ({ ...filter })),
    };
    state.effectiveRange = { start: resolved.start, end: resolved.end };
    state.finite = retainedFinite ?? idleFiniteResults();
    state.live = idleLive();
    if (source === "run" || (source === "mode" && previousExecutedQuery !== query)) {
      state.history = options.history.push(state.intent.project, query, options.clock.now());
    }
    if (hasQueryTimeFilter(query)) emitPlan("timeRange", {}, reuseExecutedSnapshot);
    else emitResolvedPlans(reuseExecutedSnapshot);
    replaceLocation(true);
    publish();
  };

  const flushLiveRows = (identity: number) => {
    if (
      state.live.identity !== identity
      || state.intent.mode !== "live"
      || state.live.status === "stopped"
      || state.live.status === "error"
    ) return;
    state.live.rate = pendingLiveRate;
    pendingLiveRate = 0;
    if (pendingLiveRows.length) {
      const combined = [...state.live.rows, ...pendingLiveRows];
      pendingLiveRows = [];
      const overflow = Math.max(0, combined.length - state.intent.liveLimit);
      if (overflow) state.live.dropped += overflow;
      state.live.rows = overflow ? combined.slice(overflow) : combined;
    }
    publish();
  };

  const connectLive = (identity: number) => {
    if (
      disposed
      || state.intent.mode !== "live"
      || state.live.identity !== identity
      || state.live.status === "stopped"
      || state.live.status === "error"
      || !state.executed
    ) return;
    state.live.status = state.live.retryAttempt > 0 ? "retrying" : "connecting";
    const fail = (failure: LogsLiveFailure) => {
      if (
        state.live.identity !== identity
        || state.intent.mode !== "live"
        || state.live.status === "stopped"
      ) return;
      liveConnection?.close();
      liveConnection = null;
      state.live.error = failure.message;
      if (!retryableLiveFailure(failure)) {
        state.live.status = "error";
        publish();
        return;
      }
      const attempt = state.live.retryAttempt;
      const baseDelay = Math.min(30_000, 1_000 * (2 ** Math.min(attempt, 10)));
      const jitter = 0.75 + options.clock.random() * 0.5;
      const delay = Math.min(30_000, Math.max(1, Math.round(baseDelay * jitter)));
      state.live.retryAttempt = attempt + 1;
      state.live.status = "retrying";
      if (liveRetryTimer !== null) options.clock.clearTimeout(liveRetryTimer);
      liveRetryTimer = options.clock.setTimeout(() => {
        liveRetryTimer = null;
        connectLive(identity);
      }, delay);
      publish();
    };
    liveConnection = options.openLiveStream({
      identity,
      project: state.executed.project,
      query: state.executed.query,
      offset: state.intent.liveOffset,
      filters: state.executed.filters.map((filter) => ({ ...filter })),
    }, {
      opened() {
        if (state.live.identity !== identity || state.live.status === "stopped") return;
        state.live.status = "open";
        state.live.error = null;
        state.live.retryAttempt = 0;
        publish();
      },
      row(nextRow) {
        if (
          state.live.identity !== identity
          || state.intent.mode !== "live"
          || state.live.status === "stopped"
          || state.live.status === "error"
        ) return;
        const fingerprint = liveRowFingerprint(nextRow);
        if (liveSeen.has(fingerprint)) return;
        liveSeen.add(fingerprint);
        const seenLimit = Math.max(1_000, state.intent.liveLimit * 4);
        while (liveSeen.size > seenLimit) {
          const oldest = liveSeen.values().next().value;
          if (oldest === undefined) break;
          liveSeen.delete(oldest);
        }
        pendingLiveRate += 1;
        pendingLiveRows.push({ ...nextRow, id: `${nextRow.id}-live-${nextLiveRowIndex++}` });
        if (pendingLiveRows.length > state.intent.liveLimit) {
          const overflow = pendingLiveRows.length - state.intent.liveLimit;
          pendingLiveRows = pendingLiveRows.slice(overflow);
          state.live.dropped += overflow;
        }
      },
      failed: fail,
    });
  };

  const startLiveExecution = (
    addHistory: boolean,
    query = state.intent.draft.trim(),
    clearRows = true,
  ) => {
    if (!query) return;
    cancelFinite();
    stopLiveResources();
    clearRefreshTimer();
    const resolved = resolveMetricsRange(state.intent.range, options.clock.now());
    const executionId = nextExecutionId++;
    const identity = nextLiveIdentity++;
    state.executed = {
      executionId,
      project: state.intent.project,
      query,
      range: { ...state.intent.range },
      start: resolved.start,
      end: resolved.end,
      limit: state.intent.limit,
      filters: state.intent.filters.map((filter) => ({ ...filter })),
    };
    state.effectiveRange = { start: resolved.start, end: resolved.end };
    state.finite = idleFiniteResults();
    if (clearRows) {
      liveSeen.clear();
      nextLiveRowIndex = 1;
    }
    state.live = {
      status: "connecting",
      identity,
      rows: clearRows ? [] : state.live.rows,
      dropped: clearRows ? 0 : state.live.dropped,
      rate: 0,
      error: null,
      retryAttempt: 0,
    };
    if (addHistory) {
      state.history = options.history.push(state.intent.project, query, options.clock.now());
    }
    replaceLocation(true);
    liveFlushTimer = options.clock.setInterval(() => flushLiveRows(identity), 1_000);
    connectLive(identity);
    publish();
  };

  const configureRefreshTimer = () => {
    clearRefreshTimer();
    if (
      state.intent.refreshSeconds <= 0
      || !state.executed
      || state.intent.mode !== "historical"
    ) return;
    refreshTimer = options.clock.setInterval(() => {
      if (disposed) return;
      const loading = Object.values(state.finite).some((slot) => slot.status === "loading");
      if (!loading) startHistoricalExecution("refresh");
    }, state.intent.refreshSeconds * 1_000);
  };

  const applyIntentExecution = () => {
    if (state.executed && state.intent.mode === "historical") {
      startHistoricalExecution("intent");
    } else {
      replaceLocation(Boolean(state.executed));
      publish();
    }
    configureRefreshTimer();
  };

  const finiteSlotByIdentity = (identity: string): LogsFiniteSlotName | null => {
    for (const slot of Object.keys(state.finite) as LogsFiniteSlotName[]) {
      if (state.finite[slot].identity === identity) return slot;
    }
    return null;
  };

  const send = (event: LogsExplorationEvent) => {
    if (disposed) return;
    switch (event.type) {
      case "activated":
        if (!state.executed && locationRequestsExecution(currentLocation)) {
          if (state.intent.mode === "live") startLiveExecution(false);
          else startHistoricalExecution("location");
          configureRefreshTimer();
        }
        break;
      case "draftChanged":
        state.intent.draft = event.draft;
        replaceLocation(false);
        publish();
        break;
      case "rangeChanged":
        state.intent.range = { ...event.range };
        applyIntentExecution();
        break;
      case "filtersChanged":
        state.intent.filters = event.filters.map((filter) => ({ ...filter }));
        if (state.executed && state.intent.mode === "live") {
          startLiveExecution(false, state.executed.query);
        } else {
          applyIntentExecution();
        }
        break;
      case "limitChanged":
        state.intent.limit = Math.max(1, Math.min(MAX_LOGS_LIMIT, Math.floor(event.limit)));
        replaceLocation(false);
        publish();
        break;
      case "refreshSecondsChanged":
        state.intent.refreshSeconds = Math.max(0, Math.min(3_600, Math.floor(event.refreshSeconds)));
        replaceLocation(Boolean(state.executed));
        configureRefreshTimer();
        publish();
        break;
      case "viewChanged":
        if (event.view === "live") {
          if (state.intent.mode !== "live") {
            const addHistory = state.executed?.query !== state.intent.draft.trim();
            state.intent.mode = "live";
            startLiveExecution(addHistory);
          }
        } else {
          const changedMode = state.intent.mode === "live";
          state.intent.mode = "historical";
          state.intent.historicalView = event.view;
          if (changedMode) startHistoricalExecution("mode");
          else {
            replaceLocation(Boolean(state.executed));
            publish();
          }
        }
        configureRefreshTimer();
        break;
      case "liveOffsetChanged":
        state.intent.liveOffset = normalizedLiveOffset(
          String(parsePrometheusDuration(event.offset) ?? 5),
        );
        if (state.executed && state.intent.mode === "live") {
          startLiveExecution(false, state.executed.query);
        } else {
          replaceLocation(Boolean(state.executed));
          publish();
        }
        break;
      case "liveLimitChanged":
        state.intent.liveLimit = Math.max(10, Math.min(50_000, Math.floor(event.limit)));
        if (state.live.rows.length > state.intent.liveLimit) {
          const overflow = state.live.rows.length - state.intent.liveLimit;
          state.live.rows = state.live.rows.slice(overflow);
          state.live.dropped += overflow;
        }
        replaceLocation(Boolean(state.executed));
        publish();
        break;
      case "configureAuxiliaryQueries":
        if (JSON.stringify(auxiliaryConfig) === JSON.stringify(event.config)) break;
        {
          const previousConfig = auxiliaryConfig;
          auxiliaryConfig = { ...event.config };
          if (state.executed && state.intent.mode === "historical" && state.effectiveRange) {
            if (auxiliaryConfig.rowsEnabled && !previousConfig.rowsEnabled) {
              emitPlan("rows", { range: state.effectiveRange });
            } else if (!auxiliaryConfig.rowsEnabled) {
              state.finite.rows = idleSlot();
            }
            if (!auxiliaryConfig.chartEnabled) {
              state.finite.hits = idleSlot();
              state.finite.stats = idleSlot();
            } else if (auxiliaryConfig.graphMode === "hits") {
              state.finite.stats = idleSlot();
              emitChartPlan("hits");
            } else {
              state.finite.hits = idleSlot();
              emitChartPlan("stats");
            }
          }
          publish();
        }
        break;
      case "run":
        if (state.intent.mode === "live") startLiveExecution(true);
        else startHistoricalExecution("run");
        configureRefreshTimer();
        break;
      case "refresh":
        if (state.executed && state.intent.mode === "historical") {
          startHistoricalExecution("refresh");
        } else if (!state.executed) {
          startHistoricalExecution("run");
        }
        configureRefreshTimer();
        break;
      case "retry": {
        if (!state.executed || state.intent.mode !== "historical") break;
        if (event.slot === "rows" || event.slot === "timeRange") {
          startHistoricalExecution("retry", state.executed);
        } else if (state.effectiveRange) {
          emitChartPlan(event.slot, true);
          publish();
        }
        break;
      }
      case "cancelFinite":
        cancelFinite();
        publish();
        break;
      case "finiteSucceeded": {
        const slot = finiteSlotByIdentity(event.identity);
        if (!slot) break;
        state.finite[slot] = {
          status: "success",
          identity: event.identity,
          data: event.data,
          error: null,
        } as never;
        if (slot === "timeRange") {
          const result = event.data as LogsQueryTimeRange;
          if (result.hasTimeFilter) {
            const start = timestampSeconds(result.start);
            const end = timestampSeconds(result.end);
            if (start !== null && end !== null && start < end) {
              state.effectiveRange = { start, end };
            }
          }
          emitResolvedPlans(true);
        }
        publish();
        break;
      }
      case "finiteFailed": {
        const slot = finiteSlotByIdentity(event.identity);
        if (!slot) break;
        state.finite[slot] = {
          status: "error",
          identity: event.identity,
          data: state.finite[slot].data,
          error: event.error,
        } as never;
        publish();
        break;
      }
      case "projectChanged": {
        if (event.project === state.intent.project) break;
        const previousExecution = state.executed;
        cancelFinite();
        stopLiveResources();
        state.intent.project = event.project;
        state.history = options.history.load(event.project);
        state.executed = null;
        state.effectiveRange = null;
        state.finite = idleFiniteResults();
        state.live = idleLive();
        if (previousExecution && state.intent.mode === "historical") {
          startHistoricalExecution("project", previousExecution);
        } else if (previousExecution && state.intent.mode === "live") {
          startLiveExecution(false, previousExecution.query);
        } else {
          publish();
        }
        configureRefreshTimer();
        break;
      }
      case "reset":
        cancelFinite();
        stopLiveResources();
        clearRefreshTimer();
        state.intent = {
          project: state.intent.project,
          draft: DEFAULT_LOGS_QUERY,
          range: { mode: "relative", presetId: "1h", seconds: 3_600 },
          filters: [],
          limit: DEFAULT_LOGS_LIMIT,
          refreshSeconds: 0,
          mode: "historical",
          historicalView: "group",
          liveOffset: "5s",
          liveLimit: 100,
        };
        state.executed = null;
        state.effectiveRange = null;
        state.finite = idleFiniteResults();
        state.live = idleLive();
        replaceLocation(false);
        publish();
        break;
      case "historyFavoriteToggled":
        state.history = options.history.toggleFavorite(state.intent.project, event.query);
        publish();
        break;
      case "historyRemoved":
        state.history = options.history.remove(state.intent.project, event.query);
        publish();
        break;
      case "historyCleared":
        state.history = options.history.clear(state.intent.project);
        publish();
        break;
      case "externalLocationChanged": {
        const nextLocation = relativeLocation(event.location);
        if (nextLocation === currentLocation) break;
        const nextIntent = parseIntent(state.intent.project, nextLocation);
        const currentRun = locationRequestsExecution(currentLocation);
        const nextRun = locationRequestsExecution(nextLocation);
        const behaviorSignature = (intent: LogsExplorationIntent, run: boolean) => JSON.stringify({
          draft: intent.draft,
          range: intent.range,
          filters: intent.filters,
          limit: intent.limit,
          mode: intent.mode,
          liveOffset: intent.mode === "live" ? intent.liveOffset : undefined,
          liveLimit: intent.mode === "live" ? intent.liveLimit : undefined,
          run,
        });
        currentLocation = nextLocation;
        if (
          behaviorSignature(state.intent, currentRun)
          === behaviorSignature(nextIntent, nextRun)
        ) {
          state.intent = nextIntent;
          replaceLocation(currentRun);
          configureRefreshTimer();
          publish();
          break;
        }
        cancelFinite();
        stopLiveResources();
        clearRefreshTimer();
        state.intent = nextIntent;
        state.executed = null;
        state.effectiveRange = null;
        state.finite = idleFiniteResults();
        state.live = idleLive();
        if (nextRun) {
          if (nextIntent.mode === "live") startLiveExecution(false);
          else startHistoricalExecution("location");
        } else {
          replaceLocation(false);
          publish();
        }
        configureRefreshTimer();
        break;
      }
      case "liveStopped":
        if (state.intent.mode !== "live" || state.live.status === "stopped") break;
        stopLiveResources();
        state.live.status = "stopped";
        state.live.rate = 0;
        state.live.error = null;
        state.live.retryAttempt = 0;
        publish();
        break;
      case "liveResumed":
        if (state.intent.mode === "live" && state.executed) {
          startLiveExecution(false, state.executed.query, true);
        }
        break;
      case "liveRetried":
        if (state.intent.mode === "live" && state.executed) {
          startLiveExecution(false, state.executed.query, false);
        }
        break;
      case "liveCleared":
        state.live.rows = [];
        state.live.dropped = 0;
        state.live.rate = 0;
        liveSeen.clear();
        pendingLiveRows = [];
        pendingLiveRate = 0;
        publish();
        break;
    }
  };

  const initialCanonical = canonicalLocation(locationRequestsExecution(currentLocation));
  if (initialCanonical !== currentLocation) {
    currentLocation = initialCanonical;
    options.onLocationReplace(initialCanonical);
  }
  if (!options.deferInitialExecution && locationRequestsExecution(options.location)) {
    if (state.intent.mode === "historical") startHistoricalExecution("location");
    else startLiveExecution(false);
  }
  configureRefreshTimer();
  publicSnapshot = snapshotOf(state);

  return {
    getSnapshot: () => publicSnapshot,
    subscribe(listener) {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
    send,
    dispose() {
      if (disposed) return;
      disposed = true;
      clearRefreshTimer();
      stopLiveResources();
      if (state.executed) options.onFiniteQueryCancel(state.executed.executionId);
      listeners.clear();
    },
  };
}

export const logsWorkspaceClock: LogsWorkspaceClock = {
  now: () => Date.now(),
  random: () => Math.random(),
  setTimeout: (callback, delay) => globalThis.setTimeout(callback, delay) as unknown as number,
  clearTimeout: (id) => globalThis.clearTimeout(id),
  setInterval: (callback, delay) => globalThis.setInterval(callback, delay) as unknown as number,
  clearInterval: (id) => globalThis.clearInterval(id),
};

export function logsLiveOffsetLabel(seconds: number): string {
  return formatPrometheusDuration(Math.max(1, Math.floor(seconds)));
}
