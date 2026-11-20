import type { Role } from "@/lib/api/auth";

/**
 * Central RBAC capability matrix and route access policy derived from the
 * authenticated user's role. Pages, sidebar, command palette, and route guards
 * all derive visibility and write controls from this single source of truth,
 * aligned with `internal/gateway/gateway.go` (enforced server-side as well).
 *
 *   admin  — full control (projects, tokens, users, channels write, AI settings, audit, diagnostics)
 *   member — alerting rules/silences/notifications/config, channels read summary, scrape targets write,
 *            issues write, issue AI analysis; read projects
 *   viewer — read-only observability & project/scrape/issue/alert-event views
 */

const ROLE_RANK: Record<Role, number> = {
  viewer: 0,
  member: 1,
  admin: 2,
};

/**
 * Fine-grained capability table aligned 1:1 with gateway RBAC middlewares in
 * `internal/gateway/gateway.go`.
 */
export const CAPABILITY_MIN_ROLE = {
  "alerts:events:read": "viewer",
  "alerts:read": "member",
  "alerts:write": "member",
  "channels:read": "member",
  "channels:write": "admin",
  "issues:read": "viewer",
  "issues:write": "member",
  "scrape:read": "viewer",
  "scrape:write": "member",
  "projects:read": "viewer",
  "projects:manage": "admin",
  "users:manage": "admin",
  "ai:manage": "admin",
  "ai:analyze": "member",
  "audit:read": "admin",
  "diagnostics:read": "admin",
} as const satisfies Record<string, Role>;

export type Capability = keyof typeof CAPABILITY_MIN_ROLE;

export const ALL_CAPABILITIES = Object.keys(CAPABILITY_MIN_ROLE) as Capability[];

export function hasMinimumRole(role: Role | null, minRole?: Role): boolean {
  if (!minRole) return true;
  if (!role) return false;
  return ROLE_RANK[role] >= ROLE_RANK[minRole];
}

export function minRoleForCapability(capability: Capability): Role {
  return CAPABILITY_MIN_ROLE[capability];
}

export function hasCapability(role: Role | null, capability?: Capability): boolean {
  if (!capability) return true;
  return hasMinimumRole(role, CAPABILITY_MIN_ROLE[capability]);
}

export interface RouteAccessRule {
  /** Canonical route path prefix (matches router.tsx & nav.ts). */
  path: string;
  /** Required capability to visit the route. Undefined means any signed-in user (viewer+). */
  capability?: Capability;
  /** Optional localized i18n title key when access is denied. */
  forbiddenTitleKey?: string;
  /** Optional localized i18n description key when access is denied. */
  forbiddenDescKey?: string;
}

/**
 * Route-level RBAC rules enforced by `RouteAccessGuard` and mirrored by `nav.ts`
 * and `CommandPalette`. Keeping these rules in one table prevents drift between
 * sidebar links, command-palette actions, and direct URL navigation.
 */
export const ROUTE_ACCESS_RULES: readonly RouteAccessRule[] = [
  {
    path: "/alerts/rules",
    capability: "alerts:read",
    forbiddenTitleKey: "common.forbiddenTitle",
    forbiddenDescKey: "alerts.rulesForbiddenDesc",
  },
  {
    path: "/alerts/silences",
    capability: "alerts:read",
    forbiddenTitleKey: "common.forbiddenTitle",
    forbiddenDescKey: "alerts.silencesForbiddenDesc",
  },
  {
    path: "/alerts/channels",
    capability: "channels:write",
    forbiddenTitleKey: "common.forbiddenTitle",
    forbiddenDescKey: "alerts.channelsForbiddenDesc",
  },
  {
    path: "/alerts/notifications",
    capability: "alerts:read",
    forbiddenTitleKey: "common.forbiddenTitle",
    forbiddenDescKey: "notifications.forbiddenDesc",
  },
  {
    path: "/settings/users",
    capability: "users:manage",
    forbiddenTitleKey: "users.forbiddenTitle",
    forbiddenDescKey: "users.forbiddenDesc",
  },
  {
    path: "/settings/audit",
    capability: "audit:read",
    forbiddenTitleKey: "audit.forbiddenTitle",
    forbiddenDescKey: "audit.forbiddenDesc",
  },
  {
    path: "/settings/diagnostics",
    capability: "diagnostics:read",
    forbiddenTitleKey: "diagnostics.forbiddenTitle",
    forbiddenDescKey: "diagnostics.forbiddenDesc",
  },
  {
    path: "/settings/ai",
    capability: "ai:manage",
    forbiddenTitleKey: "aiSettings.forbiddenTitle",
    forbiddenDescKey: "aiSettings.forbiddenDesc",
  },
];

/** Resolve the most specific route access rule matching a pathname. */
export function resolveRouteAccess(pathname: string): RouteAccessRule | undefined {
  const normalized = pathname.endsWith("/") && pathname.length > 1
    ? pathname.slice(0, -1)
    : pathname;
  return ROUTE_ACCESS_RULES.filter(
    (rule) => normalized === rule.path || normalized.startsWith(`${rule.path}/`),
  ).sort((a, b) => b.path.length - a.path.length)[0];
}

export function canAccessRoute(pathname: string, role: Role | null): boolean {
  const rule = resolveRouteAccess(pathname);
  return hasCapability(role, rule?.capability);
}

export interface GatewayRouteCapability {
  method: "GET" | "POST" | "PUT" | "PATCH" | "DELETE";
  path: string;
  capability: Capability;
}

/**
 * Authoritative mapping from Go gateway management routes (`internal/gateway/gateway.go`)
 * to frontend capabilities. Cross-checked against `gateway.go` in `permissions.test.ts`.
 */
export const GATEWAY_ROUTE_CAPABILITIES: readonly GatewayRouteCapability[] = [
  { method: "GET", path: "/api/system/error-inbox", capability: "diagnostics:read" },
  { method: "GET", path: "/api/audit", capability: "audit:read" },
  { method: "POST", path: "/api/ai/test", capability: "ai:manage" },
  { method: "GET", path: "/api/ai/settings", capability: "ai:manage" },
  { method: "PUT", path: "/api/ai/settings", capability: "ai:manage" },
  { method: "GET", path: "/api/projects", capability: "projects:read" },
  { method: "GET", path: "/api/projects/:id", capability: "projects:read" },
  { method: "GET", path: "/api/projects/:id/alert-config", capability: "projects:read" },
  { method: "PUT", path: "/api/projects/:id/alert-config", capability: "alerts:write" },
  { method: "GET", path: "/api/projects/:id/ai-config", capability: "projects:read" },
  { method: "PUT", path: "/api/projects/:id/ai-config", capability: "ai:analyze" },
  { method: "POST", path: "/api/projects", capability: "projects:manage" },
  { method: "PATCH", path: "/api/projects/:id", capability: "projects:manage" },
  { method: "PUT", path: "/api/projects/:id", capability: "projects:manage" },
  { method: "DELETE", path: "/api/projects/:id", capability: "projects:manage" },
  { method: "GET", path: "/api/projects/:id/tokens", capability: "projects:read" },
  { method: "POST", path: "/api/projects/:id/tokens", capability: "projects:manage" },
  { method: "DELETE", path: "/api/projects/:id/tokens/:tokenID", capability: "projects:manage" },
  { method: "GET", path: "/api/projects/:id/source-map-upload-tokens", capability: "projects:manage" },
  { method: "POST", path: "/api/projects/:id/source-map-upload-tokens", capability: "projects:manage" },
  { method: "DELETE", path: "/api/projects/:id/source-map-upload-tokens/:tokenID", capability: "projects:manage" },
  { method: "GET", path: "/api/users", capability: "users:manage" },
  { method: "GET", path: "/api/users/:id", capability: "users:manage" },
  { method: "POST", path: "/api/users", capability: "users:manage" },
  { method: "PATCH", path: "/api/users/:id", capability: "users:manage" },
  { method: "PUT", path: "/api/users/:id", capability: "users:manage" },
  { method: "DELETE", path: "/api/users/:id", capability: "users:manage" },
  { method: "GET", path: "/api/alert-rules", capability: "alerts:read" },
  { method: "POST", path: "/api/alert-rules", capability: "alerts:write" },
  { method: "PATCH", path: "/api/alert-rules/:id", capability: "alerts:write" },
  { method: "PUT", path: "/api/alert-rules/:id", capability: "alerts:write" },
  { method: "DELETE", path: "/api/alert-rules/:id", capability: "alerts:write" },
  { method: "GET", path: "/api/notifications", capability: "alerts:read" },
  { method: "POST", path: "/api/notifications/:id/retry", capability: "alerts:write" },
  { method: "GET", path: "/api/channels", capability: "channels:read" },
  { method: "POST", path: "/api/channels", capability: "channels:write" },
  { method: "PATCH", path: "/api/channels/:id", capability: "channels:write" },
  { method: "PUT", path: "/api/channels/:id", capability: "channels:write" },
  { method: "DELETE", path: "/api/channels/:id", capability: "channels:write" },
  { method: "POST", path: "/api/channels/:id/test", capability: "channels:write" },
  { method: "GET", path: "/api/silences", capability: "alerts:read" },
  { method: "POST", path: "/api/silences", capability: "alerts:write" },
  { method: "PATCH", path: "/api/silences/:id", capability: "alerts:write" },
  { method: "PUT", path: "/api/silences/:id", capability: "alerts:write" },
  { method: "DELETE", path: "/api/silences/:id", capability: "alerts:write" },
  { method: "GET", path: "/api/inhibition-rules", capability: "alerts:read" },
  { method: "POST", path: "/api/inhibition-rules", capability: "alerts:write" },
  { method: "PATCH", path: "/api/inhibition-rules/:id", capability: "alerts:write" },
  { method: "PUT", path: "/api/inhibition-rules/:id", capability: "alerts:write" },
  { method: "DELETE", path: "/api/inhibition-rules/:id", capability: "alerts:write" },
  { method: "GET", path: "/api/alerts", capability: "alerts:events:read" },
  { method: "GET", path: "/api/scrape-targets", capability: "scrape:read" },
  { method: "POST", path: "/api/scrape-targets", capability: "scrape:write" },
  { method: "GET", path: "/api/scrape-targets/:id", capability: "scrape:read" },
  { method: "PATCH", path: "/api/scrape-targets/:id", capability: "scrape:write" },
  { method: "PUT", path: "/api/scrape-targets/:id", capability: "scrape:write" },
  { method: "DELETE", path: "/api/scrape-targets/:id", capability: "scrape:write" },
  { method: "GET", path: "/api/scrape-targets/:id/health", capability: "scrape:read" },
  { method: "GET", path: "/api/issues", capability: "issues:read" },
  { method: "GET", path: "/api/issues/:id", capability: "issues:read" },
  { method: "GET", path: "/api/issues/:id/ai-analysis", capability: "issues:read" },
  { method: "POST", path: "/api/issues/:id/ai-analysis", capability: "ai:analyze" },
  { method: "PATCH", path: "/api/issues/:id", capability: "issues:write" },
  { method: "POST", path: "/api/issues/batch", capability: "issues:write" },
  { method: "GET", path: "/api/issues/:id/events/:event_id", capability: "issues:read" },
  { method: "DELETE", path: "/api/releases/:release/sourcemaps", capability: "projects:manage" },
];

// ── Convenience semantic predicates (all backed by CAPABILITY_MIN_ROLE) ──────

export function canManageProjects(role: Role | null): boolean {
  return hasCapability(role, "projects:manage");
}

export function canManageUsers(role: Role | null): boolean {
  return hasCapability(role, "users:manage");
}

export function canReadChannels(role: Role | null): boolean {
  return hasCapability(role, "channels:read");
}

export function canManageChannels(role: Role | null): boolean {
  return hasCapability(role, "channels:write");
}

export function canReadAlerts(role: Role | null): boolean {
  return hasCapability(role, "alerts:read");
}

export function canManageAlerts(role: Role | null): boolean {
  return hasCapability(role, "alerts:write");
}

export function canManageIssues(role: Role | null): boolean {
  return hasCapability(role, "issues:write");
}

export function canManageScrapeTargets(role: Role | null): boolean {
  return hasCapability(role, "scrape:write");
}

export function canManageAi(role: Role | null): boolean {
  return hasCapability(role, "ai:manage");
}

export function canReadAudit(role: Role | null): boolean {
  return hasCapability(role, "audit:read");
}

export function canReadDiagnostics(role: Role | null): boolean {
  return hasCapability(role, "diagnostics:read");
}

/** Generating AI issue analysis is a write action gated to member+ (not viewer). */
export function canUseAi(role: Role | null): boolean {
  return hasCapability(role, "ai:analyze");
}
