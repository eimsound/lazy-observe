import type { Role } from "@/lib/api/auth";

/**
 * Central capability checks derived from the current role. Pages hide/disable
 * management actions accordingly (and the backend enforces the same server-side
 * — this is UX, not security).
 *   admin  — full control (projects, tokens, users, channels, AI settings, alerting)
 *   member — manage alerting rules/silences/notifications/config, scrape targets, issues; read projects
 *   viewer — read-only everywhere
 */

const ROLE_RANK: Record<Role, number> = {
  viewer: 0,
  member: 1,
  admin: 2,
};

export function hasMinimumRole(role: Role | null, minRole?: Role): boolean {
  if (!minRole) return true;
  if (!role) return false;
  return ROLE_RANK[role] >= ROLE_RANK[minRole];
}

export function canManageProjects(role: Role | null): boolean {
  return role === "admin";
}

export function canManageUsers(role: Role | null): boolean {
  return role === "admin";
}

export function canManageChannels(role: Role | null): boolean {
  return role === "admin";
}

export function canManageAlerts(role: Role | null): boolean {
  return role === "admin" || role === "member";
}

export function canManageIssues(role: Role | null): boolean {
  return role === "admin" || role === "member";
}

/** Generating AI issue analysis is a write action gated to member+ (not viewer). */
export function canUseAi(role: Role | null): boolean {
  return role === "admin" || role === "member";
}
