import type { LogRow } from "../lib/api/logs.ts";
import type {
  LogsLiveConnection,
  LogsLiveFailure,
  LogsLiveObserver,
  LogsLiveRequest,
  LogsWorkspaceClock,
  MutableWorkspaceState,
} from "./logs-workspace-types.ts";

export function liveRowFingerprint(row: LogRow): string {
  const stream = row.raw._stream_id ?? row.raw._stream ?? row.stream;
  const timestamp = row.raw._time ?? row.timeRaw ?? row.time;
  return `${String(stream)}\u0000${String(timestamp)}\u0000${JSON.stringify(row.raw)}`;
}

export function retryableLiveFailure(failure: LogsLiveFailure): boolean {
  if (failure.kind === "abort" || failure.kind === "query") return false;
  if (failure.status === 401 || failure.status === 403) return false;
  if (failure.status !== undefined) {
    return failure.status === 429 || failure.status >= 500;
  }
  return failure.kind === "network" || failure.kind === "timeout";
}

export function computeLiveRetryDelay(attempt: number, randomValue: number): number {
  const baseDelay = Math.min(30_000, 1_000 * (2 ** Math.min(attempt, 10)));
  const jitter = 0.75 + randomValue * 0.5;
  return Math.min(30_000, Math.max(1, Math.round(baseDelay * jitter)));
}

export interface LogsLiveControllerDeps {
  state: MutableWorkspaceState;
  clock: LogsWorkspaceClock;
  isDisposed: () => boolean;
  openLiveStream: (
    request: LogsLiveRequest,
    observer: LogsLiveObserver,
  ) => LogsLiveConnection;
  publish: () => void;
}

export interface LogsLiveController {
  stopResources: () => void;
  beginSession: (clearRows: boolean) => number;
  connectSession: (identity: number) => void;
  stop: () => void;
  clear: () => void;
  trimToLimit: (limit: number) => void;
}

export function createLogsLiveController({
  state,
  clock,
  isDisposed,
  openLiveStream,
  publish,
}: LogsLiveControllerDeps): LogsLiveController {
  let nextLiveIdentity = 1;
  let nextLiveRowIndex = 1;
  let liveConnection: LogsLiveConnection | null = null;
  let liveRetryTimer: number | null = null;
  let liveFlushTimer: number | null = null;
  let pendingLiveRows: LogRow[] = [];
  let pendingLiveRate = 0;
  const liveSeen = new Set<string>();

  const stopResources = () => {
    liveConnection?.close();
    liveConnection = null;
    if (liveRetryTimer !== null) clock.clearTimeout(liveRetryTimer);
    if (liveFlushTimer !== null) clock.clearInterval(liveFlushTimer);
    liveRetryTimer = null;
    liveFlushTimer = null;
    pendingLiveRows = [];
    pendingLiveRate = 0;
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
      isDisposed()
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
      const delay = computeLiveRetryDelay(attempt, clock.random());
      state.live.retryAttempt = attempt + 1;
      state.live.status = "retrying";
      if (liveRetryTimer !== null) clock.clearTimeout(liveRetryTimer);
      liveRetryTimer = clock.setTimeout(() => {
        liveRetryTimer = null;
        connectLive(identity);
      }, delay);
      publish();
    };
    liveConnection = openLiveStream({
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

  return {
    stopResources,
    beginSession(clearRows: boolean): number {
      const identity = nextLiveIdentity++;
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
      return identity;
    },
    connectSession(identity: number) {
      liveFlushTimer = clock.setInterval(() => flushLiveRows(identity), 1_000);
      connectLive(identity);
    },
    stop() {
      if (state.intent.mode !== "live" || state.live.status === "stopped") return;
      stopResources();
      state.live.status = "stopped";
      state.live.rate = 0;
      state.live.error = null;
      state.live.retryAttempt = 0;
      publish();
    },
    clear() {
      state.live.rows = [];
      state.live.dropped = 0;
      state.live.rate = 0;
      liveSeen.clear();
      pendingLiveRows = [];
      pendingLiveRate = 0;
      publish();
    },
    trimToLimit(limit: number) {
      if (state.live.rows.length <= limit) return;
      const overflow = state.live.rows.length - limit;
      state.live.rows = state.live.rows.slice(overflow);
      state.live.dropped += overflow;
    },
  };
}
