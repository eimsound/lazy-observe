import assert from "node:assert/strict";
import { QueryClient } from "@tanstack/react-query";
import { test } from "vitest";

import { applyOptimisticData } from "./use-optimistic-list.ts";

test("applyOptimisticData leaves undefined cache entries untouched and updates populated entries", () => {
  assert.equal(
    applyOptimisticData<string[]>(undefined, (items) => [...items, "a"]),
    undefined,
  );
  assert.deepEqual(
    applyOptimisticData(["a", "b"], (items) => items.filter((item) => item !== "a")),
    ["b"],
  );
});

test("optimistic update and rollback preserve cached list integrity on mutation error", () => {
  const queryClient = new QueryClient();
  const key = ["issues", "prod", "open"];
  const initial = { issues: [{ id: "1" }, { id: "2" }], total: 2 };
  queryClient.setQueryData(key, initial);

  const previous = queryClient.getQueryData<typeof initial>(key);
  queryClient.setQueryData<typeof initial>(key, (old) =>
    applyOptimisticData(old, (data) => ({
      ...data,
      issues: data.issues.filter((item) => item.id !== "1"),
    })),
  );
  assert.deepEqual(queryClient.getQueryData(key), { issues: [{ id: "2" }], total: 2 });

  // Simulate rollback on error.
  if (previous !== undefined) {
    queryClient.setQueryData(key, previous);
  }
  assert.deepEqual(queryClient.getQueryData(key), initial);
});
