import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "vitest";

import type { Role } from "@/lib/api/auth";
import {
  CAPABILITY_MIN_ROLE,
  GATEWAY_ROUTE_CAPABILITIES,
  ROUTE_ACCESS_RULES,
  canAccessRoute,
  canManageAi,
  canManageAlerts,
  canManageChannels,
  canManageIssues,
  canManageProjects,
  canManageScrapeTargets,
  canManageUsers,
  canReadAlerts,
  canReadAudit,
  canReadChannels,
  canReadDiagnostics,
  canUseAi,
  hasCapability,
  hasMinimumRole,
  minRoleForCapability,
  resolveRouteAccess,
} from "./permissions.ts";
import { NAV_ITEMS, canAccessNavItem } from "../../config/nav.ts";

const SRC_ROOT = resolve(fileURLToPath(new URL("../..", import.meta.url)));
const REPO_ROOT = resolve(SRC_ROOT, "../..");

test("fine-grained RBAC capability matrix distinguishes admin, member, and viewer", () => {
  // Channels: PR #229 opens read-only minimal summary to member, write stays admin-only.
  assert.equal(hasCapability("admin", "channels:read"), true);
  assert.equal(hasCapability("member", "channels:read"), true);
  assert.equal(hasCapability("viewer", "channels:read"), false);
  assert.equal(canReadChannels("member"), true);

  assert.equal(hasCapability("admin", "channels:write"), true);
  assert.equal(hasCapability("member", "channels:write"), false);
  assert.equal(hasCapability("viewer", "channels:write"), false);
  assert.equal(canManageChannels("admin"), true);
  assert.equal(canManageChannels("member"), false);

  // Alerting rules / silences / notifications / project alert-config: member+
  assert.equal(hasCapability("viewer", "alerts:events:read"), true);
  assert.equal(canReadAlerts("admin"), true);
  assert.equal(canReadAlerts("member"), true);
  assert.equal(canReadAlerts("viewer"), false);
  assert.equal(canManageAlerts("admin"), true);
  assert.equal(canManageAlerts("member"), true);
  assert.equal(canManageAlerts("viewer"), false);

  // Issues & Scrape Targets: read viewer+, write member+
  assert.equal(hasCapability("viewer", "issues:read"), true);
  assert.equal(canManageIssues("admin"), true);
  assert.equal(canManageIssues("member"), true);
  assert.equal(canManageIssues("viewer"), false);

  assert.equal(hasCapability("viewer", "scrape:read"), true);
  assert.equal(canManageScrapeTargets("admin"), true);
  assert.equal(canManageScrapeTargets("member"), true);
  assert.equal(canManageScrapeTargets("viewer"), false);

  // Admin-only management capabilities
  assert.equal(canManageProjects("admin"), true);
  assert.equal(canManageProjects("member"), false);
  assert.equal(canManageUsers("admin"), true);
  assert.equal(canManageUsers("member"), false);
  assert.equal(canManageAi("admin"), true);
  assert.equal(canManageAi("member"), false);
  assert.equal(canReadAudit("admin"), true);
  assert.equal(canReadAudit("member"), false);
  assert.equal(canReadDiagnostics("admin"), true);
  assert.equal(canReadDiagnostics("member"), false);

  // AI issue analysis: member+
  assert.equal(canUseAi("admin"), true);
  assert.equal(canUseAi("member"), true);
  assert.equal(canUseAi("viewer"), false);
  assert.equal(hasCapability(null, "alerts:read"), false);
  assert.equal(hasCapability(null, undefined), true);
  assert.equal(hasMinimumRole(null, "member"), false);
  assert.equal(hasMinimumRole(null, undefined), true);
});

test("navigation access and route guards stay 1:1 aligned across all roles", () => {
  const byPath = new Map(NAV_ITEMS.map((item) => [item.to, item]));
  const roles: Array<Role | null> = ["admin", "member", "viewer", null];

  for (const item of NAV_ITEMS) {
    for (const role of roles) {
      assert.equal(
        canAccessNavItem(item, role),
        canAccessRoute(item.to, role),
        `nav vs route guard mismatch for ${item.to} as ${String(role)}`,
      );
    }
  }

  // Every ROUTE_ACCESS_RULES entry must correspond to a real nav item with the same capability.
  for (const rule of ROUTE_ACCESS_RULES) {
    const navItem = byPath.get(rule.path);
    assert.ok(navItem, `ROUTE_ACCESS_RULES path ${rule.path} must exist in NAV_ITEMS`);
    assert.equal(
      navItem.capability,
      rule.capability,
      `NAV_ITEMS capability for ${rule.path} must match ROUTE_ACCESS_RULES`,
    );
  }

  // Trailing slashes and nested subpaths still resolve to their route rule.
  assert.equal(resolveRouteAccess("/alerts/rules/")?.capability, "alerts:read");
  assert.equal(resolveRouteAccess("/alerts/channels/123")?.capability, "channels:write");
  assert.equal(canAccessRoute("/settings/diagnostics", "member"), false);
  assert.equal(canAccessRoute("/settings/diagnostics", "admin"), true);
});

/**
 * Extracts registered HTTP routes and their required RBAC role from
 * `internal/gateway/gateway.go` so frontend capability drift fails CI immediately.
 */
function parseGatewayRoutes(source: string): Map<string, Role> {
  const routes = new Map<string, Role>();
  const registerBody = (fnName: string): string => {
    const start = source.indexOf(`func (g *Gateway) ${fnName}()`);
    assert.ok(start >= 0, `gateway.go must define ${fnName}`);
    const openBrace = source.indexOf("{", start);
    let depth = 0;
    for (let i = openBrace; i < source.length; i += 1) {
      const ch = source[i];
      if (ch === "{") depth += 1;
      else if (ch === "}") {
        depth -= 1;
        if (depth === 0) return source.slice(openBrace + 1, i);
      }
    }
    throw new Error(`unclosed function body for ${fnName}`);
  };

  const scan = (body: string, receiver: "g.app" | "api", prefix: string) => {
    const escaped = receiver.replace(".", "\\.");
    const pattern = new RegExp(
      `${escaped}\\.(Get|Post|Put|Patch|Delete)\\(\\s*"([^"]+)"([^)]*)\\)`,
      "gs",
    );
    for (const match of body.matchAll(pattern)) {
      const method = match[1].toUpperCase();
      const fullPath = `${prefix}${match[2]}`;
      const args = match[3];
      const role: Role = args.includes("g.adminRequired")
        ? "admin"
        : args.includes("g.memberRequired")
          ? "member"
          : "viewer";
      routes.set(`${method} ${fullPath}`, role);
    }
  };

  scan(registerBody("registerPlatformRoutes"), "g.app", "");
  scan(registerBody("registerAuthRoutes"), "g.app", "");
  scan(registerBody("registerSessionAPI"), "api", "/api");
  return routes;
}

test("frontend capability matrix matches real Go RBAC routes in internal/gateway/gateway.go", () => {
  const gatewaySource = readFileSync(
    resolve(REPO_ROOT, "internal/gateway/gateway.go"),
    "utf8",
  );
  const realRoutes = parseGatewayRoutes(gatewaySource);

  for (const contract of GATEWAY_ROUTE_CAPABILITIES) {
    const key = `${contract.method} ${contract.path}`;
    const realRole = realRoutes.get(key);
    assert.ok(realRole, `gateway.go must register ${key}`);
    const frontendMinRole = minRoleForCapability(contract.capability);
    assert.equal(
      frontendMinRole,
      realRole,
      `RBAC drift on ${key}: gateway.go requires "${realRole}", frontend capability "${contract.capability}" maps to "${frontendMinRole}"`,
    );
  }

  // Ensure every adminRequired or memberRequired route in registerSessionAPI is covered.
  const coveredKeys = new Set(
    GATEWAY_ROUTE_CAPABILITIES.map((c) => `${c.method} ${c.path}`),
  );
  for (const [key, role] of realRoutes) {
    if (role === "admin" || role === "member") {
      assert.ok(
        coveredKeys.has(key),
        `privileged gateway route ${key} (${role}) must be declared in GATEWAY_ROUTE_CAPABILITIES`,
      );
    }
  }

  // Explicitly assert PR #229 contract: GET /api/channels is member, writes are admin.
  assert.equal(realRoutes.get("GET /api/channels"), "member");
  assert.equal(CAPABILITY_MIN_ROLE["channels:read"], "member");
  assert.equal(realRoutes.get("POST /api/channels"), "admin");
  assert.equal(realRoutes.get("POST /api/channels/:id/test"), "admin");
  assert.equal(CAPABILITY_MIN_ROLE["channels:write"], "admin");
});
