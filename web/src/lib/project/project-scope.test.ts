import assert from "node:assert/strict";
import { test } from "vitest";
import { QueryClient } from "@tanstack/react-query";

import { createProjectScope, switchProjectQueries, type SlugStorage } from "./project-scope.ts";

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
