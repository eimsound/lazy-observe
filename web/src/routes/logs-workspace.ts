import { MAX_LOGS_LIMIT } from "../lib/logs-explorer.ts";
import {
  parsePrometheusDuration,
  resolveMetricsRange,
} from "../lib/metrics-query.ts";
import { createLogsFiniteController } from "./logs-workspace-finite.ts";
import { createLogsLiveController } from "./logs-workspace-live.ts";
import {
  createDefaultLogsIntent,
  idleFiniteResults,
  idleLive,
  snapshotOf,
  type CreateLogsExplorationWorkspaceOptions,
  type LogsExplorationEvent,
  type LogsExplorationWorkspace,
  type LogsWorkspaceClock,
  type MutableWorkspaceState,
} from "./logs-workspace-types.ts";
import {
  buildCanonicalLogsLocation,
  executedMatchesIntent,
  intentBehaviorSignature,
  locationRequestsExecution,
  logsLiveOffsetLabel,
  normalizedLiveOffset,
  parseLogsWorkspaceIntent,
  relativeLocation,
} from "./logs-workspace-url.ts";

export { logsLiveOffsetLabel };
export type {
  CreateLogsExplorationWorkspaceOptions,
  ExecutedLogsQuery,
  LogsAuxiliaryQueryConfig,
  LogsExplorationEvent,
  LogsExplorationIntent,
  LogsExplorationMode,
  LogsExplorationSnapshot,
  LogsExplorationWorkspace,
  LogsFiniteQueryPlan,
  LogsFiniteResults,
  LogsFiniteSlotName,
  LogsHistoricalView,
  LogsHistoryAdapter,
  LogsLiveConnection,
  LogsLiveFailure,
  LogsLiveObserver,
  LogsLiveRequest,
  LogsLiveSnapshot,
  LogsResultSlot,
  LogsWorkspaceClock,
} from "./logs-workspace-types.ts";

export function createLogsExplorationWorkspace(
  options: CreateLogsExplorationWorkspaceOptions,
): LogsExplorationWorkspace {
  let currentLocation = relativeLocation(options.location);
  let nextExecutionId = 1;
  let disposed = false;
  let refreshTimer: number | null = null;
  const listeners = new Set<() => void>();

  const state: MutableWorkspaceState = {
    intent: parseLogsWorkspaceIntent(options.project, currentLocation),
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

  const replaceLocation = (run: boolean) => {
    const target = buildCanonicalLogsLocation(
      currentLocation,
      state.intent,
      run && (!state.executed || executedMatchesIntent(state.executed, state.intent)),
    );
    if (target === currentLocation) return;
    currentLocation = target;
    options.onLocationReplace(target);
  };

  const liveController = createLogsLiveController({
    state,
    clock: options.clock,
    isDisposed: () => disposed,
    openLiveStream: options.openLiveStream,
    publish,
  });

  const finiteController = createLogsFiniteController({
    state,
    clock: options.clock,
    history: options.history,
    initialAuxiliaryConfig: options.initialAuxiliaryConfig,
    nextExecutionId: () => nextExecutionId++,
    stopLiveResources: liveController.stopResources,
    replaceLocation,
    onFiniteQueryPlan: options.onFiniteQueryPlan,
    onFiniteQueryCancel: options.onFiniteQueryCancel,
    publish,
  });

  const clearRefreshTimer = () => {
    if (refreshTimer === null) return;
    options.clock.clearInterval(refreshTimer);
    refreshTimer = null;
  };

  const startLiveExecution = (
    addHistory: boolean,
    query = state.intent.draft.trim(),
    clearRows = true,
  ) => {
    if (!query) return;
    finiteController.cancel();
    liveController.stopResources();
    clearRefreshTimer();
    const resolved = resolveMetricsRange(state.intent.range, options.clock.now());
    const executionId = nextExecutionId++;
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
    const identity = liveController.beginSession(clearRows);
    if (addHistory) {
      state.history = options.history.push(state.intent.project, query, options.clock.now());
    }
    replaceLocation(true);
    liveController.connectSession(identity);
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
      if (!finiteController.isLoading()) finiteController.startExecution("refresh");
    }, state.intent.refreshSeconds * 1_000);
  };

  const applyIntentExecution = () => {
    if (state.executed && state.intent.mode === "historical") {
      finiteController.startExecution("intent");
    } else {
      replaceLocation(Boolean(state.executed));
      publish();
    }
    configureRefreshTimer();
  };

  const send = (event: LogsExplorationEvent) => {
    if (disposed) return;
    switch (event.type) {
      case "activated":
        if (!state.executed && locationRequestsExecution(currentLocation)) {
          if (state.intent.mode === "live") startLiveExecution(false);
          else finiteController.startExecution("location");
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
          if (changedMode) finiteController.startExecution("mode");
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
        liveController.trimToLimit(state.intent.liveLimit);
        replaceLocation(Boolean(state.executed));
        publish();
        break;
      case "configureAuxiliaryQueries":
        finiteController.configureAuxiliary(event.config);
        break;
      case "run":
        if (state.intent.mode === "live") startLiveExecution(true);
        else finiteController.startExecution("run");
        configureRefreshTimer();
        break;
      case "refresh":
        if (state.executed && state.intent.mode === "historical") {
          finiteController.startExecution("refresh");
        } else if (!state.executed) {
          finiteController.startExecution("run");
        }
        configureRefreshTimer();
        break;
      case "retry":
        finiteController.retrySlot(event.slot);
        break;
      case "cancelFinite":
        finiteController.cancel();
        publish();
        break;
      case "finiteSucceeded":
        finiteController.acceptSuccess(event.identity, event.data);
        break;
      case "finiteFailed":
        finiteController.acceptFailure(event.identity, event.error);
        break;
      case "projectChanged": {
        if (event.project === state.intent.project) break;
        const previousExecution = state.executed;
        finiteController.cancel();
        liveController.stopResources();
        state.intent.project = event.project;
        state.history = options.history.load(event.project);
        state.executed = null;
        state.effectiveRange = null;
        state.finite = idleFiniteResults();
        state.live = idleLive();
        if (previousExecution && state.intent.mode === "historical") {
          finiteController.startExecution("project", previousExecution);
        } else if (previousExecution && state.intent.mode === "live") {
          startLiveExecution(false, previousExecution.query);
        } else {
          publish();
        }
        configureRefreshTimer();
        break;
      }
      case "reset":
        finiteController.cancel();
        liveController.stopResources();
        clearRefreshTimer();
        state.intent = createDefaultLogsIntent(state.intent.project);
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
        const nextIntent = parseLogsWorkspaceIntent(state.intent.project, nextLocation);
        const currentRun = locationRequestsExecution(currentLocation);
        const nextRun = locationRequestsExecution(nextLocation);
        currentLocation = nextLocation;
        if (
          intentBehaviorSignature(state.intent, currentRun)
          === intentBehaviorSignature(nextIntent, nextRun)
        ) {
          state.intent = nextIntent;
          replaceLocation(currentRun);
          configureRefreshTimer();
          publish();
          break;
        }
        finiteController.cancel();
        liveController.stopResources();
        clearRefreshTimer();
        state.intent = nextIntent;
        state.executed = null;
        state.effectiveRange = null;
        state.finite = idleFiniteResults();
        state.live = idleLive();
        if (nextRun) {
          if (nextIntent.mode === "live") startLiveExecution(false);
          else finiteController.startExecution("location");
        } else {
          replaceLocation(false);
          publish();
        }
        configureRefreshTimer();
        break;
      }
      case "liveStopped":
        liveController.stop();
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
        liveController.clear();
        break;
    }
  };

  const initialCanonical = buildCanonicalLogsLocation(
    currentLocation,
    state.intent,
    locationRequestsExecution(currentLocation),
  );
  if (initialCanonical !== currentLocation) {
    currentLocation = initialCanonical;
    options.onLocationReplace(initialCanonical);
  }
  if (!options.deferInitialExecution && locationRequestsExecution(options.location)) {
    if (state.intent.mode === "historical") finiteController.startExecution("location");
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
      liveController.stopResources();
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
