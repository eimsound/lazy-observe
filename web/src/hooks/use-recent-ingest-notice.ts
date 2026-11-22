import { useCallback, useSyncExternalStore } from "react";
import { recentIngestRemainingMs } from "@/lib/ingesting-window";

// 32-bit signed setTimeout upper bound (~24.8 days) to avoid overflow firing immediately.
export const MAX_RECENT_INGEST_TIMEOUT_MS = 2_147_483_647;

export interface RecentIngestClock {
  now: () => number;
  setTimeout: (callback: () => void, delayMs: number) => ReturnType<typeof setTimeout>;
  clearTimeout: (handle: ReturnType<typeof setTimeout>) => void;
}

const DEFAULT_CLOCK: RecentIngestClock = {
  now: () => Date.now(),
  setTimeout: (callback, delayMs) => globalThis.setTimeout(callback, delayMs),
  clearTimeout: (handle) => globalThis.clearTimeout(handle),
};

export function readRecentIngestVisible(
  timestampMs: number | null,
  nowMs = DEFAULT_CLOCK.now(),
): boolean {
  if (timestampMs === null) return false;
  return recentIngestRemainingMs(timestampMs, nowMs) > 0;
}

export function subscribeToRecentIngestExpiry(
  timestampMs: number | null,
  onStoreChange: () => void,
  clock: RecentIngestClock = DEFAULT_CLOCK,
): () => void {
  if (timestampMs === null) return () => {};

  let timer: ReturnType<typeof setTimeout> | null = null;
  let disposed = false;

  const schedule = () => {
    if (disposed) return;
    const remainingMs = recentIngestRemainingMs(timestampMs, clock.now());
    if (remainingMs <= 0) return;

    const delayMs = Math.min(MAX_RECENT_INGEST_TIMEOUT_MS, Math.max(1, Math.ceil(remainingMs)));
    timer = clock.setTimeout(() => {
      timer = null;
      if (disposed) return;
      if (recentIngestRemainingMs(timestampMs, clock.now()) > 0) {
        schedule();
        return;
      }
      onStoreChange();
    }, delayMs);
  };

  schedule();

  return () => {
    disposed = true;
    if (timer !== null) {
      clock.clearTimeout(timer);
      timer = null;
    }
  };
}

function getServerSnapshot(): boolean {
  return false;
}

export function useRecentIngestNotice(timestampMs: number | null): boolean {
  const subscribe = useCallback(
    (onStoreChange: () => void) => subscribeToRecentIngestExpiry(timestampMs, onStoreChange),
    [timestampMs],
  );
  const getSnapshot = useCallback(
    () => readRecentIngestVisible(timestampMs),
    [timestampMs],
  );

  return useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);
}
