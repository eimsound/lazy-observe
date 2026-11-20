import assert from "node:assert/strict";
import { test } from "vitest";

import {
  canManageAlerts,
  canManageChannels,
  canManageIssues,
  canManageProjects,
  canManageUsers,
  canUseAi,
  hasMinimumRole,
} from "./permissions.ts";
import { NAV_ITEMS, canAccessNavItem } from "../../config/nav.ts";

test("role capability matrix distinguishes admin, member, and viewer", () => {
  assert.equal(canManageChannels("admin"), true);
  assert.equal(canManageChannels("member"), false);
  assert.equal(canManageChannels("viewer"), false);

  assert.equal(canManageAlerts("admin"), true);
  assert.equal(canManageAlerts("member"), true);
  assert.equal(canManageAlerts("viewer"), false);

  assert.equal(canManageIssues("admin"), true);
  assert.equal(canManageIssues("member"), true);
  assert.equal(canManageIssues("viewer"), false);

  assert.equal(canManageProjects("admin"), true);
  assert.equal(canManageProjects("member"), false);
  assert.equal(canManageUsers("admin"), true);
  assert.equal(canManageUsers("member"), false);
  assert.equal(canUseAi("member"), true);
  assert.equal(canUseAi("viewer"), false);
});

test("navigation access hides member/admin routes from lower roles", () => {
  const byPath = new Map(NAV_ITEMS.map((item) => [item.to, item]));

  assert.equal(canAccessNavItem(byPath.get("/alerts")!, "viewer"), true);
  assert.equal(canAccessNavItem(byPath.get("/alerts/rules")!, "viewer"), false);
  assert.equal(canAccessNavItem(byPath.get("/alerts/silences")!, "viewer"), false);
  assert.equal(canAccessNavItem(byPath.get("/alerts/notifications")!, "viewer"), false);
  assert.equal(canAccessNavItem(byPath.get("/alerts/channels")!, "viewer"), false);

  assert.equal(canAccessNavItem(byPath.get("/alerts/rules")!, "member"), true);
  assert.equal(canAccessNavItem(byPath.get("/alerts/silences")!, "member"), true);
  assert.equal(canAccessNavItem(byPath.get("/alerts/notifications")!, "member"), true);
  assert.equal(canAccessNavItem(byPath.get("/alerts/channels")!, "member"), false);
  assert.equal(canAccessNavItem(byPath.get("/settings/users")!, "member"), false);

  assert.equal(canAccessNavItem(byPath.get("/alerts/channels")!, "admin"), true);
  assert.equal(canAccessNavItem(byPath.get("/settings/users")!, "admin"), true);
  assert.equal(hasMinimumRole(null, "member"), false);
  assert.equal(hasMinimumRole(null, undefined), true);
});
