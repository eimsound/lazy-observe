import assert from "node:assert/strict";
import { test } from "vitest";

import { v8AutoSortKind, v8SortDescFirst } from "./v8-sort-parity.ts";

const rows = (count: number, value: (index: number) => unknown) =>
  Array.from({ length: count }, (_, index) => value(index));

test("v8 auto comparator ignores the first 10 rows", () => {
  assert.equal(v8AutoSortKind([]), "basic");
  assert.equal(v8AutoSortKind(rows(10, (i) => `host-${i}`)), "basic");
  assert.equal(v8AutoSortKind(rows(11, (i) => `host-${i}`)), "alphanumeric");
  assert.equal(v8AutoSortKind(rows(12, (i) => (i < 11 ? "api" : "worker"))), "text");
  assert.equal(v8AutoSortKind(rows(12, (i) => (i === 11 ? "w2" : "api"))), "alphanumeric");
  assert.equal(v8AutoSortKind(rows(12, (i) => i)), "basic");
  assert.equal(v8AutoSortKind(rows(12, () => undefined)), "basic");
  assert.equal(v8AutoSortKind(rows(12, (i) => new Date(i))), "datetime");
});

test("v8 first sort direction follows only the first row", () => {
  assert.equal(v8SortDescFirst(["a", 1]), false);
  assert.equal(v8SortDescFirst([1, "a"]), true);
  assert.equal(v8SortDescFirst([undefined, "a"]), true);
  assert.equal(v8SortDescFirst([]), true);
});
