import assert from "node:assert/strict";
import { test } from "vitest";
import {
  computeLiveRetryDelay,
  retryableLiveFailure,
} from "./logs-workspace-live.ts";
import {
  buildCanonicalLogsLocation,
  hasQueryTimeFilter,
  logsLiveOffsetLabel,
  parseLogsWorkspaceIntent,
  timestampSeconds,
} from "./logs-workspace-url.ts";

test("hasQueryTimeFilter detects _time filters at query start, whitespace, or group", () => {
  assert.equal(hasQueryTimeFilter("_time:5m | stats count()"), true);
  assert.equal(hasQueryTimeFilter("service:api _time:15m"), true);
  assert.equal(hasQueryTimeFilter("(_time:1h AND level:error)"), true);
  assert.equal(hasQueryTimeFilter("custom_time:5m"), false);
  assert.equal(hasQueryTimeFilter("*"), false);
});

test("timestampSeconds normalizes seconds, milliseconds, nanoseconds, and ISO strings", () => {
  assert.equal(timestampSeconds(1_700_000_000), 1_700_000_000);
  assert.equal(timestampSeconds(1_700_000_000_000), 1_700_000_000);
  assert.equal(timestampSeconds(1_700_000_000_000_000_000), 1_700_000_000);
  assert.equal(timestampSeconds("2027-01-15T08:00:00Z"), 1_800_000_000);
  assert.equal(timestampSeconds(""), null);
  assert.equal(timestampSeconds("not-a-time"), null);
});

test("workspace URL codec parses and canonicalizes live and historical locations", () => {
  const intent = parseLogsWorkspaceIntent(
    "alpha",
    "/logs?q=level:error&view=live&live_tailing_offset=30&rows_per_page=250&panel=detail",
  );
  assert.equal(intent.mode, "live");
  assert.equal(intent.liveOffset, "30s");
  assert.equal(intent.liveLimit, 250);
  assert.equal(logsLiveOffsetLabel(30), "30s");

  const canonical = buildCanonicalLogsLocation(
    "/logs?panel=detail&extra=1",
    intent,
    true,
  );
  const url = new URL(canonical, "https://lazy-observe.test");
  assert.equal(url.searchParams.get("q"), "level:error");
  assert.equal(url.searchParams.get("run"), "1");
  assert.equal(url.searchParams.get("view"), "live");
  assert.equal(url.searchParams.get("live_tailing_offset"), "30");
  assert.equal(url.searchParams.get("rows_per_page"), "250");
  assert.equal(url.searchParams.get("panel"), "detail");
});

test("live failure classification and retry backoff stay bounded", () => {
  assert.equal(retryableLiveFailure({ message: "429", status: 429 }), true);
  assert.equal(retryableLiveFailure({ message: "503", status: 503 }), true);
  assert.equal(retryableLiveFailure({ message: "net", kind: "network" }), true);
  assert.equal(retryableLiveFailure({ message: "timeout", kind: "timeout" }), true);
  assert.equal(retryableLiveFailure({ message: "401", status: 401 }), false);
  assert.equal(retryableLiveFailure({ message: "403", status: 403 }), false);
  assert.equal(retryableLiveFailure({ message: "parse", kind: "query" }), false);
  assert.equal(retryableLiveFailure({ message: "abort", kind: "abort" }), false);
  assert.equal(retryableLiveFailure({ message: "unknown", kind: "unknown" }), false);

  assert.equal(computeLiveRetryDelay(0, 0), 750);
  assert.equal(computeLiveRetryDelay(0, 0.5), 1_000);
  assert.equal(computeLiveRetryDelay(0, 1), 1_250);
  assert.equal(computeLiveRetryDelay(10, 1), 30_000);
});
