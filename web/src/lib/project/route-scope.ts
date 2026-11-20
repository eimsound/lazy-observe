/**
 * Classifies routes into layout-level project remount scopes so switching the
 * active project cleanly resets all local UI state (`page`, `before`,
 * `selected`, `service`, `model`, `draft`, open dialogs) on project-scoped
 * pages without remounting global management pages or URL-synced workspaces
 * that manage their own project-switch transitions.
 */

const GLOBAL_OR_SELF_MANAGED_PREFIXES = [
  "/settings",
  "/alerts/channels",
  "/logs",
  "/metrics",
  "/traces",
  "/profiling",
] as const;

function normalizePath(pathname: string): string {
  if (pathname.length > 1 && pathname.endsWith("/")) {
    return pathname.slice(0, -1);
  }
  return pathname || "/";
}

/**
 * True when switching projects should remount the route subtree at the layout
 * boundary via React `key`.
 *
 * Detail routes under `/traces/$traceID` DO remount on project change so a
 * span drawer or selection never leaks across projects, while `/traces` itself
 * clears URL search params via `useResetOnProjectChange`.
 */
export function shouldRemountOnProjectChange(pathname: string): boolean {
  const path = normalizePath(pathname);
  if (path.startsWith("/traces/") && path.length > "/traces/".length) {
    return true;
  }
  for (const prefix of GLOBAL_OR_SELF_MANAGED_PREFIXES) {
    if (path === prefix || path.startsWith(`${prefix}/`)) {
      return false;
    }
  }
  return true;
}

/**
 * Returns the React `key` applied to the authenticated layout's `<Outlet />`
 * wrapper. Changing `currentSlug` changes the key for project-scoped pages so
 * all page-local state resets atomically.
 */
export function routeProjectScopeKey(pathname: string, currentSlug: string): string {
  return shouldRemountOnProjectChange(pathname) ? `project:${currentSlug}` : "global";
}
