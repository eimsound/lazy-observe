import assert from "node:assert/strict";
import { test } from "vitest";

import {
  routeProjectScopeKey,
  shouldRemountOnProjectChange,
} from "./route-scope.ts";

test("project-scoped business pages remount cleanly when switching projects", () => {
  const projectScopedPaths = [
    "/",
    "/issues",
    "/issues/42",
    "/replays",
    "/replays/sess_1",
    "/performance",
    "/llm",
    "/infrastructure/hosts",
    "/infrastructure/hosts/node-1",
    "/infrastructure/scrape-targets",
    "/alerts",
    "/alerts/rules",
    "/alerts/silences",
    "/alerts/notifications",
    "/traces/0123456789abcdef",
  ];

  for (const path of projectScopedPaths) {
    assert.equal(shouldRemountOnProjectChange(path), true, `${path} should remount on project change`);
    assert.equal(routeProjectScopeKey(path, "prod"), "project:prod");
    assert.equal(routeProjectScopeKey(path, "staging"), "project:staging");
    assert.notEqual(
      routeProjectScopeKey(path, "prod"),
      routeProjectScopeKey(path, "staging"),
    );
  }
});

test("global management pages and URL-synced workspaces keep a stable layout key", () => {
  const stablePaths = [
    "/settings",
    "/settings/projects",
    "/settings/users",
    "/settings/audit",
    "/settings/diagnostics",
    "/settings/ai",
    "/alerts/channels",
    "/logs",
    "/metrics",
    "/traces",
    "/profiling",
  ];

  for (const path of stablePaths) {
    assert.equal(shouldRemountOnProjectChange(path), false, `${path} should keep stable layout key`);
    assert.equal(routeProjectScopeKey(path, "prod"), "global");
    assert.equal(routeProjectScopeKey(path, "staging"), "global");
  }
});
