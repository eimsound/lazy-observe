import {
  useEffect,
  useLayoutEffect,
  useState,
  useSyncExternalStore,
} from "react";
import { useQueryClient, type QueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";

import {
  queryLogHitsSeries,
  queryLogStatsRange,
  queryLogs,
  queryLogsTimeRange,
  tailLogs,
} from "../lib/api/logs.ts";
import { ApiError } from "../lib/api/client.ts";
import {
  buildLogsExtraFilterParams,
  clearLogsExplorerHistory,
  getLogsExplorerHistory,
  pushLogsExplorerHistory,
  removeLogsExplorerHistory,
  toggleLogsExplorerFavorite,
} from "../lib/logs-explorer.ts";
import {
  createLogsExplorationWorkspace,
  logsWorkspaceClock,
  type LogsAuxiliaryQueryConfig,
  type LogsExplorationWorkspace,
  type LogsFiniteQueryPlan,
  type LogsLiveFailure,
  type LogsLiveObserver,
} from "./logs-workspace.ts";

interface UseLogsWorkspaceOptions {
  project: string;
  location: string;
  auxiliaryConfig: LogsAuxiliaryQueryConfig;
  replaceLocation: (location: string) => void;
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function liveFailure(error: unknown): LogsLiveFailure {
  if (error instanceof DOMException && error.name === "AbortError") {
    return { message: error.message, kind: "abort" };
  }
  if (error instanceof ApiError) {
    return { message: error.message, status: error.status };
  }
  if (error instanceof TypeError) {
    return { message: error.message, kind: "network" };
  }
  return { message: errorMessage(error), kind: "unknown" };
}

async function executeFinitePlan(
  plan: LogsFiniteQueryPlan,
  signal: AbortSignal,
  statsErrorMessage: string,
) {
  const filters = buildLogsExtraFilterParams(plan.snapshot.filters);
  const project = plan.snapshot.project;
  switch (plan.slot) {
    case "timeRange":
      return queryLogsTimeRange({
        query: plan.snapshot.query,
        start: plan.snapshot.start,
        end: plan.snapshot.end,
        project,
        ...filters,
      }, signal);
    case "rows":
      return queryLogs({
        query: plan.snapshot.query,
        start: plan.range.start,
        end: plan.range.end,
        limit: plan.snapshot.limit,
        project,
        ...filters,
      }, signal);
    case "hits":
      return queryLogHitsSeries({
        query: plan.snapshot.query,
        start: plan.range.start,
        end: plan.range.end,
        step: plan.step,
        field: plan.groupBy,
        fieldsLimit: plan.topN,
        project,
        ...filters,
      }, signal);
    case "stats": {
      const response = await queryLogStatsRange({
        query: plan.snapshot.query,
        start: plan.range.start,
        end: plan.range.end,
        step: plan.step,
        fieldsLimit: plan.topN,
        project,
        ...filters,
      }, signal);
      if (response.status === "error") throw new Error(response.error || statsErrorMessage);
      return response;
    }
  }
}

interface LogsWorkspaceRuntimeDeps {
  replaceLocation: (location: string) => void;
  chartErrorMessage: string;
  liveStreamEndedMessage: string;
  queryClient: QueryClient;
}

interface LogsWorkspaceRuntime {
  workspace: LogsExplorationWorkspace;
  syncDeps: (next: LogsWorkspaceRuntimeDeps) => void;
  mount: () => () => void;
}

function createLogsWorkspaceRuntime(initial: {
  project: string;
  location: string;
  auxiliaryConfig: LogsAuxiliaryQueryConfig;
  deps: LogsWorkspaceRuntimeDeps;
}): LogsWorkspaceRuntime {
  let deps = initial.deps;
  const lifecycle: {
    mounted: boolean;
    mountCount: number;
    disposeTimer: number | null;
  } = { mounted: false, mountCount: 0, disposeTimer: null };
  const finiteControllers = new Map<string, {
    executionId: number;
    controller: AbortController;
  }>();

  let workspace: LogsExplorationWorkspace | null = null;
  const sendToWorkspace = (event: Parameters<LogsExplorationWorkspace["send"]>[0]) => {
    workspace?.send(event);
  };

  const runFinitePlanNow = (plan: LogsFiniteQueryPlan) => {
    const controller = new AbortController();
    finiteControllers.set(plan.identity, {
      executionId: plan.executionId,
      controller,
    });
    void deps.queryClient.fetchQuery({
      queryKey: [
        "logs-workspace",
        plan.snapshot.project,
        plan.slot,
        plan.identity,
      ],
      retry: false,
      queryFn: ({ signal }) => executeFinitePlan(
        plan,
        AbortSignal.any([signal, controller.signal]),
        deps.chartErrorMessage,
      ),
    }).then((data) => {
      sendToWorkspace({ type: "finiteSucceeded", identity: plan.identity, data });
    }).catch((error: unknown) => {
      if (controller.signal.aborted) return;
      sendToWorkspace({
        type: "finiteFailed",
        identity: plan.identity,
        error: errorMessage(error),
      });
    }).finally(() => {
      if (finiteControllers.get(plan.identity)?.controller === controller) {
        finiteControllers.delete(plan.identity);
      }
    });
  };

  const runFinitePlan = (plan: LogsFiniteQueryPlan) => {
    if (lifecycle.mounted) {
      runFinitePlanNow(plan);
      return;
    }
    queueMicrotask(() => {
      if (lifecycle.mounted) runFinitePlanNow(plan);
    });
  };

  workspace = createLogsExplorationWorkspace({
    project: initial.project,
    location: initial.location,
    clock: logsWorkspaceClock,
    history: {
      load: getLogsExplorerHistory,
      push: pushLogsExplorerHistory,
      toggleFavorite: toggleLogsExplorerFavorite,
      remove: removeLogsExplorerHistory,
      clear: clearLogsExplorerHistory,
    },
    deferInitialExecution: true,
    initialAuxiliaryConfig: initial.auxiliaryConfig,
    onLocationReplace: (nextLocation) => {
      if (lifecycle.mounted) {
        deps.replaceLocation(nextLocation);
        return;
      }
      queueMicrotask(() => {
        if (lifecycle.mounted) deps.replaceLocation(nextLocation);
      });
    },
    onFiniteQueryPlan: runFinitePlan,
    onFiniteQueryCancel: (executionId) => {
      for (const entry of finiteControllers.values()) {
        if (entry.executionId === executionId) entry.controller.abort();
      }
    },
    openLiveStream(request, observer: LogsLiveObserver) {
      const controller = new AbortController();
      const open = async () => {
        let opened = false;
        try {
          for await (const nextRow of tailLogs({
            query: request.query,
            offset: request.offset,
            refreshInterval: "1s",
            project: request.project,
            ...buildLogsExtraFilterParams(request.filters),
          }, controller.signal)) {
            if (!opened) {
              opened = true;
              observer.opened();
            }
            observer.row(nextRow);
          }
          if (!controller.signal.aborted) {
            observer.failed({ message: deps.liveStreamEndedMessage, kind: "network" });
          }
        } catch (error) {
          if (!controller.signal.aborted) observer.failed(liveFailure(error));
        }
      };
      if (lifecycle.mounted) void open();
      else {
        queueMicrotask(() => {
          if (lifecycle.mounted && !controller.signal.aborted) void open();
        });
      }
      return { close: () => controller.abort() };
    },
  });

  const activeWorkspace = workspace;

  return {
    workspace: activeWorkspace,
    syncDeps(next) {
      deps = next;
    },
    mount() {
      if (lifecycle.disposeTimer !== null) {
        globalThis.clearTimeout(lifecycle.disposeTimer);
        lifecycle.disposeTimer = null;
      }
      lifecycle.mountCount += 1;
      lifecycle.mounted = true;
      activeWorkspace.send({ type: "activated" });
      return () => {
        lifecycle.mountCount -= 1;
        lifecycle.mounted = false;
        lifecycle.disposeTimer = globalThis.setTimeout(() => {
          lifecycle.disposeTimer = null;
          if (lifecycle.mountCount > 0) return;
          activeWorkspace.dispose();
          for (const entry of finiteControllers.values()) entry.controller.abort();
          finiteControllers.clear();
        }, 0) as unknown as number;
      };
    },
  };
}

export function useLogsWorkspace({
  project,
  location,
  auxiliaryConfig,
  replaceLocation,
}: UseLogsWorkspaceOptions) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const chartErrorMessage = t("logs.chartError");
  const liveStreamEndedMessage = t("logs.liveStreamEnded");
  const [runtime] = useState(() => createLogsWorkspaceRuntime({
    project,
    location,
    auxiliaryConfig,
    deps: {
      replaceLocation,
      chartErrorMessage,
      liveStreamEndedMessage,
      queryClient,
    },
  }));
  const { workspace } = runtime;

  const snapshot = useSyncExternalStore(
    workspace.subscribe,
    workspace.getSnapshot,
    workspace.getSnapshot,
  );

  useLayoutEffect(() => {
    runtime.syncDeps({
      replaceLocation,
      chartErrorMessage,
      liveStreamEndedMessage,
      queryClient,
    });
  }, [runtime, replaceLocation, chartErrorMessage, liveStreamEndedMessage, queryClient]);

  useLayoutEffect(() => runtime.mount(), [runtime]);

  useLayoutEffect(() => {
    workspace.send({ type: "projectChanged", project });
  }, [project, workspace]);

  useEffect(() => {
    workspace.send({ type: "externalLocationChanged", location });
  }, [location, workspace]);

  useEffect(() => {
    workspace.send({ type: "configureAuxiliaryQueries", config: auxiliaryConfig });
  }, [auxiliaryConfig, workspace]);

  return { workspace, snapshot };
}
