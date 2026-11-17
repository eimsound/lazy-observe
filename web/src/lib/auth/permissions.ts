import type { Role } from "@/lib/api/auth";

/**
 * Central capability checks derived from the current role. Pages hide/disable
 * management actions accordingly (and the backend enforces the same server-side
 * — this is UX, not security).
 *   admin  — full control (projects, tokens, users, alerting)
 *   member — manage alerting (rules/channels/silences); read projects
 *   viewer — read-only everywhere
 */

export function canManageProjects(role: Role | null): boolean {
  return role === "admin";
}

export function canManageUsers(role: Role | null): boolean {
  return role === "admin";
}

export function canManageAlerts(role: Role | null): boolean {
  return role === "admin" || role === "member";
}

/** Generating AI issue analysis is a write action gated to member+ (not viewer). */
export function canUseAi(role: Role | null): boolean {
  return role === "admin" || role === "member";
}
