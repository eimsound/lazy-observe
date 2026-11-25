import assert from "node:assert/strict";
import { test } from "vitest";

import type { AlertEvent } from "@/lib/api/alerts-contract";
import { alertEventsChip, filterLinkedEvents, parseAlertEventsSearch } from "./alert-events-search.ts";

function event(id: string, rule: string, fingerprint: string, status: "firing" | "resolved" = "firing"): AlertEvent {
  return {
    id,
    fingerprint,
    rule,
    status,
    labels: {},
    annotations: {},
    starts_at: "2026-11-25T00:00:00Z",
    inhibited: false,
    updated_at: "2026-11-25T00:00:00Z",
  };
}

const events = [
  event("1", "HighErrorRate", "fp-a"),
  event("2", "HighErrorRate", "fp-b", "resolved"),
  event("3", "DiskFull", "fp-a"),
];

test("parses notification deep-link params and drops invalid values", () => {
  assert.deepEqual(parseAlertEventsSearch({ status: "firing", fingerprint: " fp-a ", rule: "DiskFull" }), {
    status: "firing",
    fingerprint: "fp-a",
    rule: "DiskFull",
  });
  assert.deepEqual(parseAlertEventsSearch({ status: "all", fingerprint: "", rule: 42 }), {
    status: undefined,
    fingerprint: undefined,
    rule: undefined,
  });
  assert.equal(parseAlertEventsSearch({ fingerprint: "x".repeat(129) }).fingerprint, undefined);
});

test("fingerprint and rule filters combine with AND", () => {
  assert.deepEqual(filterLinkedEvents(events, { fingerprint: "fp-a" }).map((e) => e.id), ["1", "3"]);
  assert.deepEqual(filterLinkedEvents(events, { fingerprint: "fp-a", rule: "DiskFull" }).map((e) => e.id), ["3"]);
  assert.deepEqual(filterLinkedEvents(events, { rule: "HighErrorRate" }).map((e) => e.id), ["1", "2"]);
  assert.deepEqual(filterLinkedEvents(events, {}).map((e) => e.id), ["1", "2", "3"]);
});

test("chip names the matched rule, falls back to the fingerprint when purged", () => {
  assert.deepEqual(alertEventsChip(events, { fingerprint: "fp-b" }), { kind: "event", rule: "HighErrorRate" });
  assert.deepEqual(alertEventsChip(events, { fingerprint: "gone" }), { kind: "fingerprint", fingerprint: "gone" });
  assert.deepEqual(alertEventsChip(events, { rule: "DiskFull" }), { kind: "rule", rule: "DiskFull" });
  assert.equal(alertEventsChip(events, {}), null);
});
