import { useEffect, useRef } from "react";
import { useProject } from "@/lib/project/project-provider";

/**
 * Pure transition detector shared by `useResetOnProjectChange` and unit tests.
 * Returns true only when the active project actually changed from a prior value.
 */
export function hasProjectChanged(previousSlug: string, nextSlug: string): boolean {
  return previousSlug !== nextSlug;
}

/**
 * Invokes `onReset(nextSlug, previousSlug)` whenever the active project slug
 * changes after initial mount. Use this on routes that stay mounted across
 * project switches (e.g. URL-synced workspaces) to clear local filters, drafts,
 * drawers, or URL query parameters cleanly.
 */
export function useResetOnProjectChange(
  onReset: (nextSlug: string, previousSlug: string) => void,
): string {
  const { currentSlug } = useProject();
  const prevRef = useRef(currentSlug);
  const callbackRef = useRef(onReset);
  callbackRef.current = onReset;

  useEffect(() => {
    const previous = prevRef.current;
    if (!hasProjectChanged(previous, currentSlug)) return;
    prevRef.current = currentSlug;
    callbackRef.current(currentSlug, previous);
  }, [currentSlug]);

  return currentSlug;
}
