import { useEffect, useRef, useState } from "react";
import type { LogsViewMode } from "@/lib/logs-explorer";
import type { LogsChartOptionsValue } from "./logs-query-controls";
import {
  parseLogsPresentationState,
  readLiveRawPreference,
  readLocalTimePreference,
  writeLiveRawPreference,
  writeLocalTimePreference,
  writeLogsPresentationParams,
  type InitialLogsState,
} from "./logs-presentation-state";

export function useLogsPresentationState(routeLocation: string) {
  const initialRef = useRef<InitialLogsState | null>(null);
  if (!initialRef.current) initialRef.current = parseLogsPresentationState(routeLocation);
  const initial = initialRef.current;

  const [localTime, setLocalTime] = useState(readLocalTimePreference);
  const [autocomplete, setAutocomplete] = useState(initial.autocomplete);
  const [hideLogs, setHideLogs] = useState(initial.hideLogs);
  const [sidebarOpen, setSidebarOpen] = useState(initial.sidebarOpen);
  const [chart, setChart] = useState<LogsChartOptionsValue>(initial.chart);
  const [groupSettings, setGroupSettings] = useState(initial.groupSettings);
  const [tableSettings, setTableSettings] = useState(initial.tableSettings);
  const [liveRaw, setLiveRaw] = useState(readLiveRawPreference);
  const [liveFollowing, setLiveFollowing] = useState(true);

  useEffect(() => {
    writeLocalTimePreference(localTime);
  }, [localTime]);

  useEffect(() => {
    writeLiveRawPreference(liveRaw);
  }, [liveRaw]);

  return {
    localTime,
    setLocalTime,
    autocomplete,
    setAutocomplete,
    hideLogs,
    setHideLogs,
    sidebarOpen,
    setSidebarOpen,
    chart,
    setChart,
    groupSettings,
    setGroupSettings,
    tableSettings,
    setTableSettings,
    liveRaw,
    setLiveRaw,
    liveFollowing,
    setLiveFollowing,
  };
}

export type LogsPresentationStateHandle = ReturnType<typeof useLogsPresentationState>;

interface UseLogsPresentationUrlSyncOptions {
  presentation: LogsPresentationStateHandle;
  routeLocation: string;
  currentHistoryHref: () => string;
  replaceLocation: (location: string) => void;
  classifyLocationChange: (location: string) => "self" | "external";
  view: LogsViewMode;
  onExternalNavigation: () => void;
}

export function useLogsPresentationUrlSync({
  presentation,
  routeLocation,
  currentHistoryHref,
  replaceLocation,
  classifyLocationChange,
  view,
  onExternalNavigation,
}: UseLogsPresentationUrlSyncOptions) {
  const {
    autocomplete,
    hideLogs,
    chart,
    groupSettings,
    tableSettings,
    setAutocomplete,
    setHideLogs,
    setChart,
    setGroupSettings,
    setTableSettings,
  } = presentation;
  const onExternalNavigationRef = useRef(onExternalNavigation);
  onExternalNavigationRef.current = onExternalNavigation;

  useEffect(() => {
    // TanStack queues history writes for a microtask, so read the router's
    // pending location instead of window.location to avoid clobbering.
    const target = writeLogsPresentationParams(currentHistoryHref(), {
      autocomplete,
      hideLogs,
      chart,
      groupSettings,
      tableSettings,
      view,
    });
    replaceLocation(target);
  }, [
    autocomplete,
    chart,
    currentHistoryHref,
    groupSettings,
    hideLogs,
    replaceLocation,
    tableSettings,
    view,
  ]);

  // Only resync presentation state on genuine external navigations (back/forward
  // or deep links), never in response to URL writes authored by this page itself.
  useEffect(() => {
    if (classifyLocationChange(routeLocation) === "self") {
      return;
    }
    const next = parseLogsPresentationState(routeLocation);
    setAutocomplete(next.autocomplete);
    setHideLogs(next.hideLogs);
    setChart(next.chart);
    setGroupSettings((current) => ({
      ...next.groupSettings,
      pageSize: next.view === "live" ? current.pageSize : next.groupSettings.pageSize,
    }));
    setTableSettings((current) => ({
      ...next.tableSettings,
      pageSize: next.view === "live" ? current.pageSize : next.tableSettings.pageSize,
    }));
    onExternalNavigationRef.current();
  }, [
    routeLocation,
    classifyLocationChange,
    setAutocomplete,
    setChart,
    setGroupSettings,
    setHideLogs,
    setTableSettings,
  ]);
}
