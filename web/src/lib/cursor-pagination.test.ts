import assert from "node:assert/strict";
import { test } from "vitest";

import {
  INITIAL_CURSOR_PAGINATION,
  canGoBackCursor,
  popCursorPage,
  pushCursorPage,
} from "./cursor-pagination.ts";

test("cursor pagination preserves page 1 (undefined) so page 2 can return to page 1", () => {
  const page1 = INITIAL_CURSOR_PAGINATION;
  assert.equal(page1.before, undefined);
  assert.equal(canGoBackCursor(page1), false);

  const page2 = pushCursorPage(page1, 500);
  assert.equal(page2.before, 500);
  assert.deepEqual(page2.history, [undefined]);
  assert.equal(canGoBackCursor(page2), true);

  const page3 = pushCursorPage(page2, 400);
  assert.equal(page3.before, 400);
  assert.deepEqual(page3.history, [undefined, 500]);

  const backToPage2 = popCursorPage(page3);
  assert.equal(backToPage2.before, 500);
  assert.deepEqual(backToPage2.history, [undefined]);
  assert.equal(canGoBackCursor(backToPage2), true);

  const backToPage1 = popCursorPage(backToPage2);
  assert.equal(backToPage1.before, undefined);
  assert.deepEqual(backToPage1.history, []);
  assert.equal(canGoBackCursor(backToPage1), false);
});

test("popping on page 1 is a safe no-op", () => {
  const same = popCursorPage(INITIAL_CURSOR_PAGINATION);
  assert.deepEqual(same, INITIAL_CURSOR_PAGINATION);
});
