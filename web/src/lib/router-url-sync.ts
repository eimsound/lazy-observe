import { useCallback, useState } from "react";
import { useRouter, useRouterState } from "@tanstack/react-router";

const DUMMY_ORIGIN = "https://lazy-observe.invalid";
const MAX_PENDING_LOCATIONS = 32;

/** Normalize a relative or absolute URL to its `pathname + search + hash`. */
export function canonicalRelativeLocation(href: string): string {
  const url = new URL(href, DUMMY_ORIGIN);
  return `${url.pathname}${url.search}${url.hash}`;
}

export interface RouterLocationSync {
  /** Record a URL written locally via `router.history.replace` / `navigate`. */
  recordSelfAuthored(href: string): string;
  /**
   * Classify a committed router location change.
   *
   * Returns `"self"` when `href` matches a pending self-authored write (pruning
   * that entry and all superseded older entries so later browser Back/Forward
   * navigations to those URLs are never misclassified as self-authored), or
   * `"external"` when the change came from browser navigation or a deep link.
   */
  classifyLocationChange(href: string): "self" | "external";
  /** Current pending queue size (for diagnostics & tests). */
  pendingCount(): number;
}

/**
 * Pure ordered tracker that distinguishes self-authored workbench URL writes
 * from genuine external navigations (browser back/forward or cross-page deep
 * links). Unlike an unbounded Set, superseded intermediate writes are pruned
 * when a newer authored write commits so navigating back to an earlier URL is
 * always detected as `"external"`.
 */
export function createRouterLocationSync(initialHref: string): RouterLocationSync {
  const initial = canonicalRelativeLocation(initialHref);
  let lastCommitted = initial;
  const pending: string[] = [];

  return {
    recordSelfAuthored(href: string): string {
      const target = canonicalRelativeLocation(href);
      if (pending[pending.length - 1] !== target) {
        pending.push(target);
        if (pending.length > MAX_PENDING_LOCATIONS) {
          pending.splice(0, pending.length - MAX_PENDING_LOCATIONS);
        }
      }
      return target;
    },

    classifyLocationChange(href: string): "self" | "external" {
      const next = canonicalRelativeLocation(href);
      if (next === lastCommitted && pending.length === 0) {
        return "self";
      }
      const matchIndex = pending.lastIndexOf(next);
      if (matchIndex >= 0) {
        pending.splice(0, matchIndex + 1);
        lastCommitted = next;
        return "self";
      }
      pending.length = 0;
      lastCommitted = next;
      return "external";
    },

    pendingCount(): number {
      return pending.length;
    },
  };
}

/**
 * TanStack Router single-source-of-truth helper for workbenches that mirror
 * state to URL search params without bypassing the router via
 * `window.history.replaceState` or reading `window.location.search`.
 */
export function useRouterLocationSync() {
  const router = useRouter();
  const routeLocation = useRouterState({
    select: (state) => canonicalRelativeLocation(state.location.href),
  });
  const [sync] = useState<RouterLocationSync>(() => createRouterLocationSync(routeLocation));
  const [initialPathname] = useState(() => new URL(routeLocation, DUMMY_ORIGIN).pathname);

  const replaceLocation = useCallback(
    (href: string) => {
      const target = canonicalRelativeLocation(href);
      const current = canonicalRelativeLocation(router.history.location.href);
      if (target === current) return;
      sync.recordSelfAuthored(target);
      router.history.replace(target, router.history.location.state);
    },
    [router, sync],
  );

  const recordSelfAuthored = useCallback((href: string) => {
    sync.recordSelfAuthored(href);
  }, [sync]);

  const classifyLocationChange = useCallback(
    (href: string) => sync.classifyLocationChange(href),
    [sync],
  );

  const subscribeExternalLocation = useCallback(
    (onExternal: (nextLocation: string) => void) =>
      router.history.subscribe(({ location }) => {
        const next = canonicalRelativeLocation(location.href);
        if (new URL(next, DUMMY_ORIGIN).pathname !== initialPathname) return;
        if (sync.classifyLocationChange(next) === "self") return;
        onExternal(next);
      }),
    [initialPathname, router, sync],
  );

  const currentHistoryHref = useCallback(
    () => canonicalRelativeLocation(router.history.location.href),
    [router],
  );

  return {
    router,
    routeLocation,
    currentHistoryHref,
    replaceLocation,
    recordSelfAuthored,
    classifyLocationChange,
    subscribeExternalLocation,
  };
}
