import assert from "node:assert/strict";
import { test } from "vitest";

import { queryViewState } from "./query-view-state.ts";

const CORE_QUERY_SURFACES = [
  "metrics",
  "logs",
  "traces",
  "issues",
  "replay",
  "profiles",
  "performance",
] as const;

for (const surface of CORE_QUERY_SURFACES) {
  test(`${surface} distinguishes pending, empty, error, stale, and ready`, () => {
    assert.equal(queryViewState({ hasData: false, isLoading: true, isError: false, isEmpty: false }), "pending");
    assert.equal(queryViewState({ hasData: false, isLoading: false, isError: true, isEmpty: false }), "error");
    assert.equal(queryViewState({ hasData: true, isLoading: false, isError: false, isEmpty: true }), "empty");
    assert.equal(queryViewState({ hasData: true, isLoading: false, isError: false, isEmpty: false }), "ready");
    assert.equal(queryViewState({ hasData: true, isLoading: false, isError: true, isEmpty: false }), "stale");
  });
}

test("a successful retry clears stale without changing the retained-data contract", () => {
  const retained = { rows: ["stable-row"], filter: "service:checkout" };
  const stale = queryViewState({ hasData: true, isLoading: false, isError: true, isEmpty: false });
  const recovered = queryViewState({ hasData: true, isLoading: false, isError: false, isEmpty: false });

  assert.equal(stale, "stale");
  assert.equal(recovered, "ready");
  assert.deepEqual(retained, { rows: ["stable-row"], filter: "service:checkout" });
});
