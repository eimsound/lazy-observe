import type {
  LogRow,
  LogsHitsResponse,
  LogsQueryTimeRange,
  LogsStatsQueryRangeResponse,
} from "../lib/api/logs.ts";
import {
  DEFAULT_LOGS_LIMIT,
  DEFAULT_LOGS_QUERY,
  type LogsExtraFilter,
  type LogsHistoryEntry,
} from "../lib/logs-explorer.ts";
import type { MetricsRange } from "../lib/metrics-query.ts";

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

export interface LogsFinitePlanBase {
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

export interface MutableWorkspaceState {
  intent: LogsExplorationIntent;
  executed: ExecutedLogsQuery | null;
  effectiveRange: { start: number; end: number } | null;
  finite: LogsFiniteResults;
  live: LogsLiveSnapshot;
  history: LogsHistoryEntry[];
}

export const DEFAULT_AUXILIARY_CONFIG: LogsAuxiliaryQueryConfig = {
  rowsEnabled: true,
  chartEnabled: false,
  graphMode: "hits",
  groupBy: "",
  topN: 5,
  step: null,
};

export function idleSlot<T>(): LogsResultSlot<T> {
  return { status: "idle", identity: null, data: null, error: null };
}

export function idleFiniteResults(): LogsFiniteResults {
  return {
    timeRange: idleSlot(),
    rows: idleSlot(),
    hits: idleSlot(),
    stats: idleSlot(),
  };
}

export function idleLive(): LogsLiveSnapshot {
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

export function createDefaultLogsIntent(project: string): LogsExplorationIntent {
  return {
    project,
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
}

export function cloneIntent(intent: LogsExplorationIntent): LogsExplorationIntent {
  return {
    ...intent,
    range: { ...intent.range },
    filters: intent.filters.map((filter) => ({ ...filter })),
  };
}

export function snapshotOf(state: MutableWorkspaceState): LogsExplorationSnapshot {
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
