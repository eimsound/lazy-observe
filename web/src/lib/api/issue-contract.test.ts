import assert from "node:assert/strict";
import { test } from "vitest";

import {
  normalizeErrorEvent,
  normalizeIssueDetail,
  normalizeIssueListResponse,
  type ErrorEventDTO,
  type IssueDetailDTO,
  type IssueListResponseDTO,
} from "./issue-contract.ts";

const summary = {
  id: 42,
  fingerprint: "fp-42",
  title: "TypeError: checkout failed",
  level: "error" as const,
  status: "open" as const,
  count: 7,
  user_count: 3,
  first_seen: 1_752_758_400_000,
  last_seen: 1_752_762_000_000,
  trend: [0, 1, 2, 4],
};

test("normalizes the gateway issue list and numeric IDs", () => {
  const response: IssueListResponseDTO = {
    issues: [summary],
    total: 1,
    page: 1,
    page_size: 50,
  };
  assert.deepEqual(normalizeIssueListResponse(response), {
    issues: [{ ...summary, id: "42" }],
    total: 1,
    page: 1,
    page_size: 50,
  });
});

test("normalizes detail event summaries without accepting RFC3339 timestamps", () => {
  const detail: IssueDetailDTO = {
    ...summary,
    culprit: "submitOrder(src/checkout.ts)",
    events: [{
      event_id: "event-1",
      timestamp: 1_752_762_000_000,
      trace_id: "trace-1",
      session_id: "session-1",
    }],
  };
  assert.equal(normalizeIssueDetail(detail).events[0]?.timestamp, 1_752_762_000_000);
  assert.throws(
    () => normalizeIssueDetail({
      ...detail,
      events: [{ ...detail.events[0], timestamp: "2025-07-17T13:00:00Z" as never }],
    }),
    /epoch milliseconds/,
  );
});

test("normalizes a real event shape and requires crash-safe contexts and tags", () => {
  const event: ErrorEventDTO = {
    event_id: "event-1",
    timestamp: 1_752_762_000_000,
    level: "error",
    platform: "browser",
    exception: {
      type: "TypeError",
      value: "checkout failed",
      stacktrace: {
        frames: [{ function: "submitOrder", filename: "src/checkout.ts", lineno: 42, in_app: true }],
      },
    },
    contexts: { trace: { trace_id: "trace-1" } },
    session_id: "session-1",
    tags: { release: "web@1" },
  };
  assert.deepEqual(normalizeErrorEvent(event).tags, { release: "web@1" });
  assert.throws(() => normalizeErrorEvent({ ...event, contexts: undefined as never }), /contexts/);
  assert.throws(() => normalizeErrorEvent({ ...event, tags: undefined as never }), /tags/);
});

test("rejects legacy summaries missing a real trend or pagination metadata", () => {
  assert.throws(
    () => normalizeIssueListResponse({ issues: [{ ...summary, trend: undefined as never }], total: 1, page: 1, page_size: 50 }),
    /trend/,
  );
  assert.throws(
    () => normalizeIssueListResponse({ issues: [summary], page: 1, page_size: 50 } as never),
    /total/,
  );
});

test("normalizes non-canonical SDK levels such as fatal, warn, and debug without breaking the list", () => {
  const list = normalizeIssueListResponse({
    issues: [
      { ...summary, id: 1, level: "fatal" as never },
      { ...summary, id: 2, level: "warn" as never },
      { ...summary, id: 3, level: "debug" as never },
      { ...summary, id: 4, level: "CRITICAL" as never },
    ],
    total: 4,
    page: 1,
    page_size: 50,
  });

  assert.deepEqual(
    list.issues.map((item) => item.level),
    ["error", "warning", "info", "error"],
  );
  assert.throws(
    () => normalizeIssueListResponse({ issues: [{ ...summary, level: "" as never }], total: 1, page: 1, page_size: 50 }),
    /issue level is invalid/,
  );
});
