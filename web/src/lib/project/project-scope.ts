import type { QueryClient } from "@tanstack/react-query";

/**
 * Project scope for data reads (`X-LO-Project`). Kept free of Vite/React so it
 * runs under Vitest in the node environment (`pnpm test`).
 */

export const PROJECT_STORAGE_KEY = "lo-project";

export interface SlugStorage {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
  removeItem(key: string): void;
}

export interface ProjectScope {
  get(): string;
  set(slug: string): void;
}

/**
 * The selected project lives in this tab's memory, seeded once from storage.
 * Storage is shared by every tab, so it only provides the default for tabs
 * opened later; a switch in one tab never retargets another tab's requests
 * behind its (unchanged) UI.
 */
export function createProjectScope(storage: SlugStorage | null): ProjectScope {
  let current = "";
  try {
    current = storage?.getItem(PROJECT_STORAGE_KEY) ?? "";
  } catch {
    current = "";
  }
  return {
    get() {
      return current;
    },
    set(slug) {
      current = slug;
      try {
        if (slug) storage?.setItem(PROJECT_STORAGE_KEY, slug);
        else storage?.removeItem(PROJECT_STORAGE_KEY);
      } catch {
        /* storage unavailable: the tab still keeps its own scope */
      }
    },
  };
}

/**
 * Build a project-scoped React Query key while keeping the domain root at
 * index 0 so root-prefix invalidations (e.g. `["issues"]`, `["scrape-targets"]`)
 * continue to work and every project keeps an isolated cache entry.
 */
export function projectQueryKey<const TQueryKey extends readonly [string, ...unknown[]]>(
  project: string,
  queryKey: TQueryKey,
): readonly [TQueryKey[0], string, ...unknown[]] {
  const [root, ...rest] = queryKey;
  return [root, project, ...rest];
}

/**
 * Query roots that must survive a project switch: global data, and roots whose
 * keys already carry the project (the logs workspace owns those executions and
 * their cancellation itself, see ADR 0008).
 */
const UNAFFECTED_ROOTS = new Set(["auth", "projects", "logs-workspace", "logs-explorer"]);

function isProjectScoped(queryKey: readonly unknown[]): boolean {
  return !UNAFFECTED_ROOTS.has(String(queryKey[0]));
}

export interface ProjectSelectionResolution {
  /** True once `currentSlug` is already valid for the loaded project list. */
  ready: boolean;
  /** The valid slug to use (empty when the user has no projects). */
  resolvedSlug: string;
  /** True when the stored/active slug must be updated to `resolvedSlug`. */
  changed: boolean;
  /**
   * True when a `?project=` link names a project that is not in the list
   * (deleted, renamed, or not accessible). Children stay unmounted.
   */
  linkUnavailable?: boolean;
}

/** Search parameter carried by console links (alert notifications). */
export const PROJECT_LINK_PARAM = "project";

/** The `?project=` slug from a raw search string (`?a=b`), or "". */
export function readProjectLinkParam(searchStr: string): string {
  return new URLSearchParams(searchStr).get(PROJECT_LINK_PARAM)?.trim() ?? "";
}

/** The raw search string without `?project=` (keeps other params), "" when empty. */
export function stripProjectLinkParam(searchStr: string): string {
  // Split the raw string instead of re-serialising URLSearchParams, which
  // would re-encode the remaining params (`%20` -> `+`).
  const rest = searchStr
    .replace(/^\?/, "")
    .split("&")
    .filter((pair) => pair !== "" && pair.split("=")[0] !== PROJECT_LINK_PARAM);
  return rest.length ? `?${rest.join("&")}` : "";
}

/**
 * Resolve the active project slug against the authoritative `/api/projects`
 * list. When a stale slug is cached in localStorage (deleted/renamed project),
 * `ready` stays false until state and storage switch to `resolvedSlug` so
 * child routes never fire data queries with an invalid `X-LO-Project` header.
 *
 * A `linkSlug` (from `?project=`) takes precedence. An unknown link slug never
 * falls back silently: `/issues/42?project=gone` must not open another
 * project's Issue #42.
 */
export function resolveProjectSelection(
  projects: readonly { slug: string }[],
  currentSlug: string,
  linkSlug = "",
): ProjectSelectionResolution {
  if (linkSlug) {
    if (!projects.some((p) => p.slug === linkSlug)) {
      return { ready: false, resolvedSlug: currentSlug, changed: false, linkUnavailable: true };
    }
    return {
      ready: currentSlug === linkSlug,
      resolvedSlug: linkSlug,
      changed: currentSlug !== linkSlug,
    };
  }
  if (projects.length === 0) {
    return {
      ready: currentSlug === "",
      resolvedSlug: "",
      changed: currentSlug !== "",
    };
  }
  if (currentSlug && projects.some((p) => p.slug === currentSlug)) {
    return {
      ready: true,
      resolvedSlug: currentSlug,
      changed: false,
    };
  }
  return {
    ready: false,
    resolvedSlug: projects[0].slug,
    changed: true,
  };
}

/**
 * Bring cached queries in line with a newly selected project. Query keys do
 * not carry the project, so results cached under the previous project are
 * cancelled and reset (not merely invalidated): an invalidated query keeps
 * showing its old data while it refetches, and an in-flight request sent with
 * the old X-LO-Project header could otherwise land in the cache afterwards.
 */
export async function switchProjectQueries(client: QueryClient): Promise<void> {
  const filters = { predicate: (query: { queryKey: readonly unknown[] }) => isProjectScoped(query.queryKey) };
  await client.cancelQueries(filters);
  await client.resetQueries(filters);
}
