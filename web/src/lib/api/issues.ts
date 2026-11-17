import { apiGet, apiSend, IS_MOCK, API_ROOT } from "./client";
import {
  normalizeErrorEvent,
  normalizeIssueDetail,
  normalizeIssueListResponse,
  normalizeIssueSummary,
  type ErrorEvent,
  type ErrorEventDTO,
  type IssueDetail,
  type IssueDetailDTO,
  type IssueLevel,
  type IssueListResponseDTO,
  type IssueListResult,
  type IssueSort,
  type IssueStatus,
  type IssueSummary,
  type IssueSummaryDTO,
} from "./issue-contract";

// Mock data is only for VITE_MOCK builds; load it lazily so it never ships with real chunks.
const loadMockIssues = () => import("./mock-issues");

export { ISSUE_LEVELS, ISSUE_STATUSES } from "./issue-contract";
export type {
  ErrorEvent,
  ErrorEventDTO,
  IssueDetail,
  IssueDetailDTO,
  IssueEventSummary,
  IssueEventSummaryDTO,
  IssueLevel,
  IssueListResponseDTO,
  IssueListResult,
  IssueSort,
  IssueStatus,
  IssueSummary,
  IssueSummaryDTO,
  StackFrame,
  StackTrace,
} from "./issue-contract";

/**
 * Issues (error-tracking) API. The gateway wire DTO is validated here before
 * the page sees it; notably numeric database IDs become route-safe strings.
 */

export interface IssueListParams {
  status: IssueStatus;
  level?: IssueLevel | "";
  q?: string;
  sort?: IssueSort;
  page?: number;
}

export async function listIssues(
  params: IssueListParams,
  signal?: AbortSignal,
): Promise<IssueListResult> {
  if (IS_MOCK) return (await loadMockIssues()).mockListIssues(params);
  const response = await apiGet<IssueListResponseDTO>("/issues", {
    base: API_ROOT,
    params: {
      status: params.status,
      level: params.level || undefined,
      q: params.q || undefined,
      sort: params.sort || undefined,
      page: params.page || undefined,
    },
    signal,
  });
  return normalizeIssueListResponse(response);
}

export async function getIssue(id: string, signal?: AbortSignal): Promise<IssueDetail> {
  if (IS_MOCK) return (await loadMockIssues()).mockGetIssue(id);
  const response = await apiGet<IssueDetailDTO>(`/issues/${encodeURIComponent(id)}`, {
    base: API_ROOT,
    signal,
  });
  return normalizeIssueDetail(response);
}

export async function updateIssueStatus(
  id: string,
  status: IssueStatus,
): Promise<IssueSummary> {
  if (IS_MOCK) return (await loadMockIssues()).mockUpdateIssueStatus(id, status);
  const response = await apiSend<IssueSummaryDTO>(
    "PATCH",
    `/issues/${encodeURIComponent(id)}`,
    { status },
    { base: API_ROOT },
  );
  return normalizeIssueSummary(response);
}

/** Apply one status to many issues in a single request. */
export async function batchUpdateIssueStatus(ids: string[], status: IssueStatus): Promise<number> {
  if (IS_MOCK) {
    for (const id of ids) await (await loadMockIssues()).mockUpdateIssueStatus(id, status);
    return ids.length;
  }
  const response = await apiSend<{ updated: number }>(
    "POST",
    "/issues/batch",
    { ids: ids.map(Number), status },
    { base: API_ROOT },
  );
  return response.updated;
}

export async function getIssueEvent(
  issueId: string,
  eventId: string,
  signal?: AbortSignal,
): Promise<ErrorEvent> {
  if (IS_MOCK) return (await loadMockIssues()).mockGetIssueEvent(issueId, eventId);
  const response = await apiGet<ErrorEventDTO>(
    `/issues/${encodeURIComponent(issueId)}/events/${encodeURIComponent(eventId)}`,
    { base: API_ROOT, signal },
  );
  return normalizeErrorEvent(response);
}
