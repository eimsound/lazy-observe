import assert from "node:assert/strict";
import { test } from "vitest";

import { normalizeReplaySessionDetail } from "./replay-contract.ts";

const detail = {
  session_id: "session-1",
  started_at: 1_721_234_567_000,
  last_seen_at: 1_721_234_570_000,
  segment_count: 1,
  total_bytes: 1024,
  has_error: true,
  release: "2026.7.18",
  page_url: "/checkout",
  segments: [{ seq: 0, started_at: 1_721_234_567_000, event_count: 5, size_bytes: 1024 }],
  markers: [
    { type: "error", time: 1_721_234_567_500, event_id: "event-1", title: "checkout failed", level: "error" },
    { type: "http", time: 1_721_234_568_000, method: "POST", path: "/api/orders", status: 503, duration_ms: 12.5 },
    { type: "vital", time: 1_721_234_569_000, name: "LCP", value: 2450.5, rating: "needs-improvement" },
  ],
};

test("normalizes error, HTTP and Web Vital Replay markers", () => {
  const normalized = normalizeReplaySessionDetail(detail);
  assert.deepEqual(normalized.markers.map((marker) => marker.type), ["error", "http", "vital"]);
  assert.deepEqual(normalized.markers[1], {
    type: "http",
    time: 1_721_234_568_000,
    method: "POST",
    path: "/api/orders",
    status: 503,
    duration_ms: 12.5,
  });
  assert.equal("error_markers" in normalized, false);
});

test("rejects the legacy detail shape without a unified marker stream", () => {
  const { markers: _markers, ...legacy } = detail;
  assert.throws(() => normalizeReplaySessionDetail(legacy), /must contain markers/);
});

test("rejects HTTP marker URLs that could expose query secrets", () => {
  const unsafe = structuredClone(detail);
  unsafe.markers[1] = {
    type: "http",
    time: 1_721_234_568_000,
    method: "GET",
    path: "/api/orders?token=secret",
    status: 200,
    duration_ms: 5,
  };
  assert.throws(() => normalizeReplaySessionDetail(unsafe), /must not contain a URL query/);
});
