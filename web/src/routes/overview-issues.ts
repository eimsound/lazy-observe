import type {
  IssueListParams,
  IssueListResult,
  IssueSummary,
} from "../lib/api/issues.ts";

export const RECENT_ISSUES_LIMIT = 3;

export const RECENT_ISSUES_PARAMS = {
  status: "open",
  sort: "last_seen",
  page: 1,
} as const satisfies IssueListParams;

export type RecentIssuesView =
  | { status: "pending" }
  | { status: "error"; message: string }
  | { status: "empty" }
  | { status: "ready"; issues: IssueSummary[] };

/**
 * Convert TanStack Query's result into the four UI states rendered by the
 * Overview card. Keeping this decision pure makes the pending/error/empty
 * contract testable without a browser DOM.
 */
export function recentIssuesView(result: {
  isPending: boolean;
  isError: boolean;
  data?: IssueListResult;
  error?: unknown;
}): RecentIssuesView {
  if (result.isPending) return { status: "pending" };
  if (result.isError) {
    return {
      status: "error",
      message: result.error instanceof Error ? result.error.message : "",
    };
  }

  const issues = result.data?.issues.slice(0, RECENT_ISSUES_LIMIT) ?? [];
  return issues.length === 0 ? { status: "empty" } : { status: "ready", issues };
}
