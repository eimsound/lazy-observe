import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import dayjs from "@/lib/dayjs";
import { DEFAULT_RANGE_ID, rangeById, resolveRange } from "@/lib/api/ranges";

const useIsomorphicLayoutEffect =
  typeof window !== "undefined" ? useLayoutEffect : useEffect;

export interface ResolvedTimeWindow {
  /** Unix epoch seconds. */
  start: number;
  /** Unix epoch seconds. */
  end: number;
  /** Step interval in seconds. */
  step: number;
}

export interface CustomTimeWindow {
  /** Unix epoch seconds. */
  start: number;
  /** Unix epoch seconds. */
  end: number;
}

export const CUSTOM_TIME_RANGE_ID = "__custom__";

/**
 * Pure monotonic timestamp advancer: ensures every manual Refresh / Retry
 * advances the query window `end` by at least +1 second even if clicked
 * multiple times within the same wall-clock second.
 */
export function advanceWindowNowMs(
  previousEndSeconds: number,
  nowMs = dayjs().valueOf(),
): number {
  return Math.max((previousEndSeconds + 1) * 1000, nowMs);
}

/**
 * Pure resolver for a range preset id (or optional custom window).
 */
export function resolveTimeWindow(
  rangeId: string,
  nowMs = dayjs().valueOf(),
  customWindow?: CustomTimeWindow | null,
): ResolvedTimeWindow {
  if (rangeId === CUSTOM_TIME_RANGE_ID && customWindow) {
    const span = Math.max(1, customWindow.end - customWindow.start);
    return {
      start: customWindow.start,
      end: customWindow.end,
      step: Math.max(1, Math.floor(span / 120)),
    };
  }
  return resolveRange(rangeById(rangeId), nowMs);
}

export interface UseTimeWindowOptions {
  /** Optional custom absolute window (activated when `rangeId === CUSTOM_TIME_RANGE_ID`). */
  customWindow?: CustomTimeWindow | null;
}

export interface TimeWindowController {
  rangeId: string;
  setRangeId: (nextRangeId: string) => void;
  window: ResolvedTimeWindow;
  /**
   * Synchronous ref reader for pages (such as PerformancePage) that keep
   * `queryKey` stable across refreshes so HTTP 500 errors retain prior rows in
   * `stale` state and Retry reuses the already-advanced window.
   */
  getWindow: () => ResolvedTimeWindow;
  /**
   * Advances `end` monotonically by at least 1 second (updating both the ref
   * synchronously and React state) and returns the new window.
   */
  advance: () => ResolvedTimeWindow;
}

/**
 * Shared time-window controller for observability & performance pages.
 * Eliminates frozen `useMemo(..., [])` windows and duplicate `nowMs` refresh
 * boilerplate while preserving synchronous ref access for stale-on-error flows.
 */
export function useTimeWindow(
  initialRangeId = DEFAULT_RANGE_ID,
  options?: UseTimeWindowOptions,
): TimeWindowController {
  const customWindow = options?.customWindow ?? null;
  const [rangeId, setRangeIdState] = useState(initialRangeId);
  const [nowMs, setNowMs] = useState(() => dayjs().valueOf());

  const window = useMemo(
    () => resolveTimeWindow(rangeId, nowMs, customWindow),
    [rangeId, nowMs, customWindow],
  );

  const windowRef = useRef<ResolvedTimeWindow>(window);
  const rangeIdRef = useRef(rangeId);
  const customWindowRef = useRef(customWindow);

  useIsomorphicLayoutEffect(() => {
    windowRef.current = window;
    rangeIdRef.current = rangeId;
    customWindowRef.current = customWindow;
  }, [window, rangeId, customWindow]);

  const getWindow = useCallback(() => windowRef.current, []);

  const setRangeId = useCallback((nextRangeId: string) => {
    const nextNow = dayjs().valueOf();
    rangeIdRef.current = nextRangeId;
    windowRef.current = resolveTimeWindow(nextRangeId, nextNow, customWindowRef.current);
    setRangeIdState(nextRangeId);
    setNowMs(nextNow);
  }, []);

  const advance = useCallback(() => {
    const nextNow = advanceWindowNowMs(windowRef.current.end, dayjs().valueOf());
    const nextWindow = resolveTimeWindow(rangeIdRef.current, nextNow, customWindowRef.current);
    windowRef.current = nextWindow;
    setNowMs(nextNow);
    return nextWindow;
  }, []);

  return { rangeId, setRangeId, window, getWindow, advance };
}
