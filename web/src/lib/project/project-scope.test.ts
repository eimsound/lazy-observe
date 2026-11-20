import assert from "node:assert/strict";
import { test } from "vitest";
import { QueryClient } from "@tanstack/react-query";

import {
  createProjectScope,
  projectQueryKey,
  resolveProjectSelection,
  switchProjectQueries,
  type SlugStorage,
} from "./project-scope.ts";

function memoryStorage(): SlugStorage {
  const values = new Map<string, string>();
  return {
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => void values.set(key, value),
    removeItem: (key) => void values.delete(key),
  };
}

test("each tab keeps its own project; storage only seeds new tabs", () => {
  const shared = memoryStorage(); // localStorage is shared by every tab
  const tabA = createProjectScope(shared);
  tabA.set("alpha");
  const tabB = createProjectScope(shared);
  assert.equal(tabB.get(), "alpha");

  tabA.set("beta");
  // Tab B's UI still shows alpha, so its requests must still target alpha.
  assert.equal(tabB.get(), "alpha");
  assert.equal(tabA.get(), "beta");
  assert.equal(createProjectScope(shared).get(), "beta");
});

test("switching project drops every project-scoped cached result", async () => {
  const client = new QueryClient();
  client.setQueryData(["issues", { page: 1 }], ["alpha issue"]);
  client.setQueryData(["auth", "me"], { username: "u" });
  client.setQueryData(["projects"], [{ slug: "alpha" }, { slug: "beta" }]);
  // Already keyed by project and lifecycle-managed by the logs workspace.
  client.setQueryData(["logs-workspace", "beta", "rows", "id-1"], ["beta row"]);

  await switchProjectQueries(client);

  assert.deepEqual(client.getQueryData(["logs-workspace", "beta", "rows", "id-1"]), ["beta row"]);

  assert.equal(client.getQueryData(["issues", { page: 1 }]), undefined);
  assert.deepEqual(client.getQueryData(["auth", "me"]), { username: "u" });
  assert.equal((client.getQueryData(["projects"]) as unknown[]).length, 2);
});

test("resolveProjectSelection defers rendering until a stale or missing project slug is corrected", () => {
  const projects = [{ slug: "alpha" }, { slug: "beta" }];

  assert.deepEqual(resolveProjectSelection(projects, "deleted-project"), {
    ready: false,
    resolvedSlug: "alpha",
    changed: true,
  });
  assert.deepEqual(resolveProjectSelection(projects, ""), {
    ready: false,
    resolvedSlug: "alpha",
    changed: true,
  });
  assert.deepEqual(resolveProjectSelection(projects, "beta"), {
    ready: true,
    resolvedSlug: "beta",
    changed: false,
  });
  assert.deepEqual(resolveProjectSelection([], "stale"), {
    ready: false,
    resolvedSlug: "",
    changed: true,
  });
  assert.deepEqual(resolveProjectSelection([], ""), {
    ready: true,
    resolvedSlug: "",
    changed: false,
  });
});

test("projectQueryKey preserves domain root at index 0 and isolates cache entries by project", () => {
  const alphaKey = projectQueryKey("alpha", ["issues", "open", 1]);
  const betaKey = projectQueryKey("beta", ["issues", "open", 1]);

  assert.deepEqual(alphaKey, ["issues", "alpha", "open", 1]);
  assert.deepEqual(betaKey, ["issues", "beta", "open", 1]);
  assert.equal(alphaKey[0], "issues");
  assert.notDeepEqual(alphaKey, betaKey);

  const client = new QueryClient();
  client.setQueryData(alphaKey, ["alpha-only"]);
  client.setQueryData(betaKey, ["beta-only"]);
  assert.deepEqual(client.getQueryData(alphaKey), ["alpha-only"]);
  assert.deepEqual(client.getQueryData(betaKey), ["beta-only"]);
});
