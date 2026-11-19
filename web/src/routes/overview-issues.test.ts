import assert from "node:assert/strict";
import { test } from "vitest";

import type { IssueListResult, IssueSummary } from "../lib/api/issues.ts";
import {
  RECENT_ISSUES_LIMIT,
  RECENT_ISSUES_PARAMS,
  recentIssuesView,
} from "./overview-issues.ts";

function issue(id: number): IssueSummary {
  return {
    id: String(id),
    fingerprint: `fingerprint-${id}`,
    title: `Issue ${id}`,
    level: "error",
    status: "open",
    count: id,
    user_count: 1,
    first_seen: id,
    last_seen: id,
    trend: [id],
  };
}

function result(issues: IssueSummary[]): IssueListResult {
  return { issues, total: issues.length, page: 1, page_size: 15 };
}

test("requests the newest open issues from the real Issues API contract", () => {
  assert.deepEqual(RECENT_ISSUES_PARAMS, {
    status: "open",
    sort: "last_seen",
    page: 1,
  });
});

test("maps pending, error and empty query results to distinct card states", () => {
  assert.deepEqual(
    recentIssuesView({ isPending: true, isError: false }),
    { status: "pending" },
  );
  assert.deepEqual(
    recentIssuesView({
      isPending: false,
      isError: true,
      error: new Error("gateway unavailable"),
    }),
    { status: "error", message: "gateway unavailable" },
  );
  assert.deepEqual(
    recentIssuesView({ isPending: false, isError: false, data: result([]) }),
    { status: "empty" },
  );
});

test("keeps only the recent issues displayed by the overview card", () => {
  const issues = Array.from({ length: RECENT_ISSUES_LIMIT + 2 }, (_, index) => issue(index + 1));
  assert.deepEqual(
    recentIssuesView({ isPending: false, isError: false, data: result(issues) }),
    { status: "ready", issues: issues.slice(0, RECENT_ISSUES_LIMIT) },
  );
});
