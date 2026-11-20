import {
  useEffect,
  useLayoutEffect,
  useRef,
  useSyncExternalStore,
} from "react";
import { useQueryClient } from "@tanstack/react-query";
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

export function useLogsWorkspace({
  project,
  location,
  auxiliaryConfig,
  replaceLocation,
}: UseLogsWorkspaceOptions) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const workspaceRef = useRef<LogsExplorationWorkspace | null>(null);
  const lifecycle = useRef<{
    mounted: boolean;
    mountCount: number;
    disposeTimer: number | null;
  }>({ mounted: false, mountCount: 0, disposeTimer: null });
  const finiteControllers = useRef(new Map<string, {
    executionId: number;
    controller: AbortController;
  }>());

  if (!workspaceRef.current) {
    const sendToWorkspace = (event: Parameters<LogsExplorationWorkspace["send"]>[0]) => {
      workspaceRef.current?.send(event);
    };
    const runFinitePlanNow = (plan: LogsFiniteQueryPlan) => {
      const controller = new AbortController();
      finiteControllers.current.set(plan.identity, {
        executionId: plan.executionId,
        controller,
      });
      void queryClient.fetchQuery({
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
          t("logs.chartError"),
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
        if (finiteControllers.current.get(plan.identity)?.controller === controller) {
          finiteControllers.current.delete(plan.identity);
        }
      });
    };
    const runFinitePlan = (plan: LogsFiniteQueryPlan) => {
      if (lifecycle.current.mounted) {
        runFinitePlanNow(plan);
        return;
      }
      queueMicrotask(() => {
        if (lifecycle.current.mounted) runFinitePlanNow(plan);
      });
    };

    workspaceRef.current = createLogsExplorationWorkspace({
      project,
      location,
      clock: logsWorkspaceClock,
      history: {
        load: getLogsExplorerHistory,
        push: pushLogsExplorerHistory,
        toggleFavorite: toggleLogsExplorerFavorite,
        remove: removeLogsExplorerHistory,
        clear: clearLogsExplorerHistory,
      },
      deferInitialExecution: true,
      initialAuxiliaryConfig: auxiliaryConfig,
      onLocationReplace: (nextLocation) => {
        if (lifecycle.current.mounted) {
          replaceLocation(nextLocation);
          return;
        }
        queueMicrotask(() => {
          if (lifecycle.current.mounted) replaceLocation(nextLocation);
        });
      },
      onFiniteQueryPlan: runFinitePlan,
      onFiniteQueryCancel: (executionId) => {
        for (const entry of finiteControllers.current.values()) {
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
              observer.failed({ message: t("logs.liveStreamEnded"), kind: "network" });
            }
          } catch (error) {
            if (!controller.signal.aborted) observer.failed(liveFailure(error));
          }
        };
        if (lifecycle.current.mounted) void open();
        else {
          queueMicrotask(() => {
            if (lifecycle.current.mounted && !controller.signal.aborted) void open();
          });
        }
        return { close: () => controller.abort() };
      },
    });
  }

  const workspace = workspaceRef.current;
  const snapshot = useSyncExternalStore(
    workspace.subscribe,
    workspace.getSnapshot,
    workspace.getSnapshot,
  );

  useLayoutEffect(() => {
    // Both refs hold one mutable object for the hook's lifetime; capture them
    // so the deferred cleanup provably works on the same objects.
    const state = lifecycle.current;
    const controllers = finiteControllers.current;
    if (state.disposeTimer !== null) {
      globalThis.clearTimeout(state.disposeTimer);
      state.disposeTimer = null;
    }
    state.mountCount += 1;
    state.mounted = true;
    workspace.send({ type: "activated" });
    return () => {
      state.mountCount -= 1;
      state.mounted = false;
      state.disposeTimer = globalThis.setTimeout(() => {
        state.disposeTimer = null;
        if (state.mountCount > 0) return;
        workspace.dispose();
        for (const entry of controllers.values()) entry.controller.abort();
        controllers.clear();
      }, 0) as unknown as number;
    };
  }, [workspace]);

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
