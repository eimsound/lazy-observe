import type { LogsQueryTimeRange } from "../lib/api/logs.ts";
import { MAX_LOGS_LIMIT } from "../lib/logs-explorer.ts";
import { resolveMetricsRange } from "../lib/metrics-query.ts";
import {
  DEFAULT_AUXILIARY_CONFIG,
  idleFiniteResults,
  idleLive,
  idleSlot,
  type ExecutedLogsQuery,
  type LogsAuxiliaryQueryConfig,
  type LogsFinitePlanBase,
  type LogsFiniteQueryPlan,
  type LogsFiniteSlotName,
  type LogsHistoryAdapter,
  type LogsWorkspaceClock,
  type MutableWorkspaceState,
} from "./logs-workspace-types.ts";
import {
  hasQueryTimeFilter,
  timestampSeconds,
} from "./logs-workspace-url.ts";

export type HistoricalExecutionSource =
  | "run"
  | "refresh"
  | "retry"
  | "project"
  | "location"
  | "mode"
  | "intent";

export interface LogsFiniteControllerDeps {
  state: MutableWorkspaceState;
  clock: LogsWorkspaceClock;
  history: LogsHistoryAdapter;
  initialAuxiliaryConfig?: LogsAuxiliaryQueryConfig;
  nextExecutionId: () => number;
  stopLiveResources: () => void;
  replaceLocation: (run: boolean) => void;
  onFiniteQueryPlan: (plan: LogsFiniteQueryPlan) => void;
  onFiniteQueryCancel: (executionId: number) => void;
  publish: () => void;
}

export interface LogsFiniteController {
  cancel: () => void;
  startExecution: (
    source: HistoricalExecutionSource,
    snapshot?: ExecutedLogsQuery | null,
  ) => void;
  configureAuxiliary: (config: LogsAuxiliaryQueryConfig) => void;
  retrySlot: (slot: LogsFiniteSlotName) => void;
  acceptSuccess: (identity: string, data: unknown) => void;
  acceptFailure: (identity: string, error: string) => void;
  isLoading: () => boolean;
}

export function createLogsFiniteController({
  state,
  clock,
  history,
  initialAuxiliaryConfig,
  nextExecutionId,
  stopLiveResources,
  replaceLocation,
  onFiniteQueryPlan,
  onFiniteQueryCancel,
  publish,
}: LogsFiniteControllerDeps): LogsFiniteController {
  let auxiliaryConfig = {
    ...DEFAULT_AUXILIARY_CONFIG,
    ...initialAuxiliaryConfig,
  };
  let nextPlanAttempt = 1;

  const isLoading = () => (
    Object.values(state.finite).some((slot) => slot.status === "loading")
  );

  const cancel = () => {
    if (!state.executed) return;
    if (isLoading()) onFiniteQueryCancel(state.executed.executionId);
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
    onFiniteQueryPlan({
      executionId: executed.executionId,
      identity,
      snapshot: { ...executed, range: { ...executed.range }, filters: [...executed.filters] },
      slot,
      ...plan,
    } as LogsFiniteQueryPlan);
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

  const emitResolvedPlans = (preserveData = false) => {
    const range = state.effectiveRange;
    if (!range) return;
    if (auxiliaryConfig.rowsEnabled) emitPlan("rows", { range }, preserveData);
    if (auxiliaryConfig.chartEnabled) emitChartPlan(auxiliaryConfig.graphMode, preserveData);
  };

  const slotByIdentity = (identity: string): LogsFiniteSlotName | null => {
    for (const slot of Object.keys(state.finite) as LogsFiniteSlotName[]) {
      if (state.finite[slot].identity === identity) return slot;
    }
    return null;
  };

  const startExecution = (
    source: HistoricalExecutionSource,
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
    cancel();
    stopLiveResources();
    const executionRange = reuseExecutedSnapshot && snapshot
      ? snapshot.range
      : state.intent.range;
    const resolved = resolveMetricsRange(executionRange, clock.now());
    const executionId = nextExecutionId();
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
      state.history = history.push(state.intent.project, query, clock.now());
    }
    if (hasQueryTimeFilter(query)) emitPlan("timeRange", {}, reuseExecutedSnapshot);
    else emitResolvedPlans(reuseExecutedSnapshot);
    replaceLocation(true);
    publish();
  };

  return {
    cancel,
    startExecution,
    isLoading,
    configureAuxiliary(config) {
      if (JSON.stringify(auxiliaryConfig) === JSON.stringify(config)) return;
      const previousConfig = auxiliaryConfig;
      auxiliaryConfig = { ...config };
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
    },
    retrySlot(slot) {
      if (!state.executed || state.intent.mode !== "historical") return;
      if (slot === "rows" || slot === "timeRange") {
        startExecution("retry", state.executed);
      } else if (state.effectiveRange) {
        emitChartPlan(slot, true);
        publish();
      }
    },
    acceptSuccess(identity, data) {
      const slot = slotByIdentity(identity);
      if (!slot) return;
      state.finite[slot] = {
        status: "success",
        identity,
        data,
        error: null,
      } as never;
      if (slot === "timeRange") {
        const result = data as LogsQueryTimeRange;
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
    },
    acceptFailure(identity, error) {
      const slot = slotByIdentity(identity);
      if (!slot) return;
      state.finite[slot] = {
        status: "error",
        identity,
        data: state.finite[slot].data,
        error,
      } as never;
      publish();
    },
  };
}
