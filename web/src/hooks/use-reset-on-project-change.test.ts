import assert from "node:assert/strict";
import { test } from "vitest";

import { hasProjectChanged } from "./use-reset-on-project-change.ts";

test("hasProjectChanged only fires when the slug actually transitions", () => {
  assert.equal(hasProjectChanged("production", "production"), false);
  assert.equal(hasProjectChanged("", ""), false);
  assert.equal(hasProjectChanged("production", "staging"), true);
  assert.equal(hasProjectChanged("staging", ""), true);
});
