import assert from "node:assert/strict";
import { test } from "vitest";

import {
  canonicalRelativeLocation,
  createRouterLocationSync,
} from "./router-url-sync.ts";

test("canonicalRelativeLocation strips origins and normalizes relative hrefs", () => {
  assert.equal(canonicalRelativeLocation("/logs?q=*&view=table"), "/logs?q=*&view=table");
  assert.equal(
    canonicalRelativeLocation("https://example.com/metrics?g0.expr=up#top"),
    "/metrics?g0.expr=up#top",
  );
});

test("rapid self-authored URL updates prune superseded entries so browser Back is detected as external", () => {
  const sync = createRouterLocationSync("/logs?q=*&view=table");

  // Two rapid local edits happen before TanStack Router commits the final microtask.
  sync.recordSelfAuthored("/logs?q=*&view=table&rows_per_page=50");
  sync.recordSelfAuthored("/logs?q=*&view=table&rows_per_page=200");
  assert.equal(sync.pendingCount(), 2);

  // Router coalesces and commits only the latest URL.
  assert.equal(sync.classifyLocationChange("/logs?q=*&view=table&rows_per_page=200"), "self");
  // Superseded entry (`rows_per_page=50`) must be pruned, not left in a Set forever.
  assert.equal(sync.pendingCount(), 0);

  // Later browser Back to `rows_per_page=50` MUST be classified as external.
  assert.equal(sync.classifyLocationChange("/logs?q=*&view=table&rows_per_page=50"), "external");
});

test("repeated local commits without change never trigger an external resync that would reset drawers or pagination", () => {
  const sync = createRouterLocationSync("/logs?q=error&run=1&view=table");

  assert.equal(sync.classifyLocationChange("/logs?q=error&run=1&view=table"), "self");

  sync.recordSelfAuthored("/logs?q=error&run=1&view=table&hide_chart=1");
  assert.equal(sync.classifyLocationChange("/logs?q=error&run=1&view=table&hide_chart=1"), "self");
  assert.equal(sync.classifyLocationChange("/logs?q=error&run=1&view=table&hide_chart=1"), "self");
});

test("external navigation clears stale pending self-authored writes", () => {
  const sync = createRouterLocationSync("/metrics?g0.expr=up");

  sync.recordSelfAuthored("/metrics?g0.expr=up&g0.tab=graph");
  // User clicks a deep link before the local replace lands.
  assert.equal(
    sync.classifyLocationChange("/metrics?q=rate(http_requests_total[5m])&run=1&evidence=contextual"),
    "external",
  );
  assert.equal(sync.pendingCount(), 0);
});
