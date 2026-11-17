export type IssueLevel = "error" | "warning" | "info";
export const ISSUE_LEVELS: IssueLevel[] = ["error", "warning", "info"];

export type IssueStatus = "open" | "resolved" | "ignored";
export const ISSUE_STATUSES: IssueStatus[] = ["open", "resolved", "ignored"];

export type IssueSort = "last_seen" | "count";

/** Gateway issue summary DTO. IDs remain numeric on the wire. */
export interface IssueSummaryDTO {
  id: number;
  fingerprint: string;
  title: string;
  level: IssueLevel;
  status: IssueStatus;
  count: number;
  user_count: number;
  /** Epoch milliseconds. */
  first_seen: number;
  /** Epoch milliseconds. */
  last_seen: number;
  trend: number[];
}

export interface IssueEventSummaryDTO {
  event_id: string;
  /** Epoch milliseconds. */
  timestamp: number;
  trace_id?: string;
  session_id?: string;
}

export interface IssueDetailDTO extends IssueSummaryDTO {
  culprit: string;
  events: IssueEventSummaryDTO[];
}

export interface IssueListResponseDTO {
  issues: IssueSummaryDTO[];
  total: number;
  page: number;
  page_size: number;
}

export interface StackFrame {
  function: string;
  filename: string;
  lineno?: number;
  colno?: number;
  in_app: boolean;
  context_line?: string;
}

export interface StackTrace {
  frames: StackFrame[];
}

export interface ErrorEventDTO {
  event_id: string;
  timestamp: number;
  level: IssueLevel;
  platform?: string;
  release?: string;
  environment?: string;
  message?: string;
  exception: {
    type: string;
    value: string;
    stacktrace: StackTrace;
    raw_stacktrace?: StackTrace;
  };
  contexts: {
    trace?: { trace_id: string; span_id?: string };
    browser?: { name: string; version: string };
    os?: { name: string; version: string };
  };
  session_id?: string;
  tags: Record<string, string>;
  url?: string;
  user?: { id?: string; ip?: string };
}

/** Browser-safe summary after validating the gateway boundary. */
export interface IssueSummary extends Omit<IssueSummaryDTO, "id"> {
  id: string;
}

export type IssueEventSummary = IssueEventSummaryDTO;

export interface IssueDetail extends IssueSummary {
  culprit: string;
  events: IssueEventSummary[];
}

export type ErrorEvent = ErrorEventDTO;

export interface IssueListResult {
  issues: IssueSummary[];
  total: number;
  page: number;
  page_size: number;
}

function object(value: unknown, label: string): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new TypeError(`${label} must be an object`);
  }
  return value as Record<string, unknown>;
}

function text(value: unknown, label: string, allowEmpty = false): string {
  if (typeof value !== "string" || (!allowEmpty && value.trim() === "")) {
    throw new TypeError(`${label} must be a${allowEmpty ? "" : " non-empty"} string`);
  }
  return value;
}

function optionalText(value: unknown, label: string): string | undefined {
  return value === undefined ? undefined : text(value, label, true);
}

function integer(value: unknown, label: string, minimum = 0): number {
  if (!Number.isSafeInteger(value) || (value as number) < minimum) {
    throw new TypeError(`${label} must be an integer >= ${minimum}`);
  }
  return value as number;
}

function epochMillis(value: unknown, label: string): number {
  if (typeof value !== "number" || !Number.isFinite(value) || value < 0) {
    throw new TypeError(`${label} must be epoch milliseconds`);
  }
  return value;
}

function issueLevel(value: unknown): IssueLevel {
  if (value !== "error" && value !== "warning" && value !== "info") {
    throw new TypeError("issue level is invalid");
  }
  return value;
}

function issueStatus(value: unknown): IssueStatus {
  if (value !== "open" && value !== "resolved" && value !== "ignored") {
    throw new TypeError("issue status is invalid");
  }
  return value;
}

export function normalizeIssueSummary(dto: IssueSummaryDTO): IssueSummary {
  const row = object(dto, "issue");
  if (!Array.isArray(row.trend)) {
    throw new TypeError("issue trend must be an array");
  }
  const firstSeen = epochMillis(row.first_seen, "issue first_seen");
  const lastSeen = epochMillis(row.last_seen, "issue last_seen");
  if (lastSeen < firstSeen) {
    throw new TypeError("issue last_seen precedes first_seen");
  }
  return {
    id: String(integer(row.id, "issue id", 1)),
    fingerprint: text(row.fingerprint, "issue fingerprint"),
    title: text(row.title, "issue title"),
    level: issueLevel(row.level),
    status: issueStatus(row.status),
    count: integer(row.count, "issue count"),
    user_count: integer(row.user_count, "issue user_count"),
    first_seen: firstSeen,
    last_seen: lastSeen,
    trend: row.trend.map((value, index) => integer(value, `issue trend[${index}]`)),
  };
}

function normalizeEventSummary(value: unknown): IssueEventSummary {
  const event = object(value, "issue event summary");
  const traceId = optionalText(event.trace_id, "issue trace_id");
  const sessionId = optionalText(event.session_id, "issue session_id");
  return {
    event_id: text(event.event_id, "issue event_id"),
    timestamp: epochMillis(event.timestamp, "issue event timestamp"),
    ...(traceId ? { trace_id: traceId } : {}),
    ...(sessionId ? { session_id: sessionId } : {}),
  };
}

export function normalizeIssueListResponse(response: IssueListResponseDTO): IssueListResult {
  const envelope = object(response, "issue list response");
  if (!Array.isArray(envelope.issues)) {
    throw new TypeError("issue list response must contain issues");
  }
  return {
    issues: envelope.issues.map((row) => normalizeIssueSummary(row as IssueSummaryDTO)),
    total: integer(envelope.total, "issue total"),
    page: integer(envelope.page, "issue page", 1),
    page_size: integer(envelope.page_size, "issue page_size", 1),
  };
}

export function normalizeIssueDetail(dto: IssueDetailDTO): IssueDetail {
  const row = object(dto, "issue detail");
  if (!Array.isArray(row.events)) {
    throw new TypeError("issue detail must contain events");
  }
  return {
    ...normalizeIssueSummary(dto),
    culprit: text(row.culprit, "issue culprit"),
    events: row.events.map(normalizeEventSummary),
  };
}

function normalizeStackTrace(value: unknown, label: string): StackTrace {
  const stack = object(value, label);
  if (!Array.isArray(stack.frames)) {
    throw new TypeError(`${label} must contain frames`);
  }
  return {
    frames: stack.frames.map((value, index) => {
      const frame = object(value, `${label} frame[${index}]`);
      if (typeof frame.in_app !== "boolean") {
        throw new TypeError(`${label} frame[${index}] in_app must be boolean`);
      }
      const contextLine = optionalText(frame.context_line, `${label} frame context_line`);
      return {
        function: text(frame.function, `${label} frame function`, true),
        filename: text(frame.filename, `${label} frame filename`, true),
        ...(frame.lineno === undefined ? {} : { lineno: integer(frame.lineno, `${label} frame lineno`) }),
        ...(frame.colno === undefined ? {} : { colno: integer(frame.colno, `${label} frame colno`) }),
        in_app: frame.in_app,
        ...(contextLine ? { context_line: contextLine } : {}),
      };
    }),
  };
}

function normalizeClientContext(value: unknown, label: string): { name: string; version: string } {
  const context = object(value, label);
  return {
    name: text(context.name, `${label} name`, true),
    version: text(context.version, `${label} version`, true),
  };
}

export function normalizeErrorEvent(dto: ErrorEventDTO): ErrorEvent {
  const event = object(dto, "error event");
  const exception = object(event.exception, "error event exception");
  const contexts = object(event.contexts, "error event contexts");
  const tags = object(event.tags, "error event tags");
  const normalizedTags: Record<string, string> = {};
  for (const [key, value] of Object.entries(tags)) {
    normalizedTags[key] = text(value, `error event tag ${key}`, true);
  }

  const trace = contexts.trace === undefined ? undefined : object(contexts.trace, "error event trace");
  const user = event.user === undefined ? undefined : object(event.user, "error event user");
  const rawStack = exception.raw_stacktrace === undefined
    ? undefined
    : normalizeStackTrace(exception.raw_stacktrace, "error event raw stacktrace");
  const platform = optionalText(event.platform, "error event platform");
  const release = optionalText(event.release, "error event release");
  const environment = optionalText(event.environment, "error event environment");
  const message = optionalText(event.message, "error event message");
  const sessionId = optionalText(event.session_id, "error event session_id");
  const eventURL = optionalText(event.url, "error event url");
  return {
    event_id: text(event.event_id, "error event event_id"),
    timestamp: epochMillis(event.timestamp, "error event timestamp"),
    level: issueLevel(event.level),
    ...(platform ? { platform } : {}),
    ...(release ? { release } : {}),
    ...(environment ? { environment } : {}),
    ...(message ? { message } : {}),
    exception: {
      type: text(exception.type, "error event exception type", true),
      value: text(exception.value, "error event exception value", true),
      stacktrace: normalizeStackTrace(exception.stacktrace, "error event stacktrace"),
      ...(rawStack ? { raw_stacktrace: rawStack } : {}),
    },
    contexts: {
      ...(trace ? {
        trace: {
          trace_id: text(trace.trace_id, "error event trace_id"),
          ...(optionalText(trace.span_id, "error event span_id") ? { span_id: trace.span_id as string } : {}),
        },
      } : {}),
      ...(contexts.browser === undefined ? {} : { browser: normalizeClientContext(contexts.browser, "error event browser") }),
      ...(contexts.os === undefined ? {} : { os: normalizeClientContext(contexts.os, "error event os") }),
    },
    ...(sessionId ? { session_id: sessionId } : {}),
    tags: normalizedTags,
    ...(eventURL ? { url: eventURL } : {}),
    ...(user ? {
      user: {
        ...(optionalText(user.id, "error event user id") ? { id: user.id as string } : {}),
        ...(optionalText(user.ip, "error event user ip") ? { ip: user.ip as string } : {}),
      },
    } : {}),
  };
}
