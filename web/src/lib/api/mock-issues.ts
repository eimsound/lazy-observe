import type {
  ErrorEvent,
  IssueDetail,
  IssueEventSummary,
  IssueLevel,
  IssueListParams,
  IssueListResult,
  IssueStatus,
  IssueSummary,
  StackFrame,
} from "./issues";
import { hexId, makeRng, type Rng } from "./mock-random";
import { errorSessionIds, sessionMeta } from "./mock-sessions";
import dayjs from "../dayjs.ts";

/**
 * In-memory Issues store for VITE_MOCK mode. Issues + their error events are
 * generated once (deterministically) at module load; `status` is mutable for
 * the tab lifetime so resolve/ignore/reopen persist. Error events carry the
 * same `session_id` the Replay mock uses (see mock-sessions) so the "play
 * replay" button lands on a real session and its error marker aligns on the
 * timeline; likewise `trace_id` resolves in the (pure-function) trace mock.
 */

const BASE_NOW = dayjs().valueOf();
const ISSUE_COUNT = 24;
const PAGE_SIZE = 15;

interface IssueTemplate {
  type: string;
  value: string;
  level: IssueLevel;
}

const TEMPLATES: IssueTemplate[] = [
  { type: "TypeError", value: "Cannot read properties of undefined (reading 'map')", level: "error" },
  { type: "TypeError", value: "undefined is not a function", level: "error" },
  { type: "ReferenceError", value: "process is not defined", level: "error" },
  { type: "RangeError", value: "Maximum call stack size exceeded", level: "error" },
  { type: "Error", value: "Network request failed: 503 Service Unavailable", level: "error" },
  { type: "ChunkLoadError", value: "Loading chunk 42 failed", level: "error" },
  { type: "Error", value: "Hydration failed because the initial UI does not match", level: "error" },
  { type: "TypeError", value: "Failed to fetch", level: "error" },
  { type: "Warning", value: "Each child in a list should have a unique \"key\" prop", level: "warning" },
  { type: "Warning", value: "Can't perform a React state update on an unmounted component", level: "warning" },
  { type: "DeprecationWarning", value: "componentWillReceiveProps has been renamed", level: "warning" },
  { type: "Info", value: "Feature flag \"new-checkout\" evaluated to control", level: "info" },
];

const APP_FILES = [
  "src/features/checkout/CheckoutForm.tsx",
  "src/features/orders/useOrders.ts",
  "src/components/DataTable.tsx",
  "src/lib/api/client.ts",
  "src/hooks/useSession.ts",
];
const APP_FNS = [
  "handleSubmit",
  "renderRow",
  "fetchOrders",
  "normalizeResponse",
  "onClick",
  "mapItems",
  "parsePayload",
  "loadProfile",
];
const VENDOR_FRAMES: Omit<StackFrame, "context_line">[] = [
  {
    function: "commitHookEffectListMount",
    filename: "node_modules/react-dom/cjs/react-dom.production.min.js",
    lineno: 24501,
    colno: 15,
    in_app: false,
  },
  {
    function: "invokeGuardedCallbackDev",
    filename: "node_modules/react-dom/cjs/react-dom.production.min.js",
    lineno: 3945,
    colno: 12,
    in_app: false,
  },
  {
    function: "workLoopSync",
    filename: "node_modules/scheduler/cjs/scheduler.production.min.js",
    lineno: 533,
    colno: 21,
    in_app: false,
  },
];

const BROWSERS = [
  { name: "Chrome", version: "126.0.0" },
  { name: "Safari", version: "17.5" },
  { name: "Firefox", version: "127.0" },
  { name: "Edge", version: "125.0" },
];
const OSES = [
  { name: "macOS", version: "14.5" },
  { name: "Windows", version: "11" },
  { name: "iOS", version: "17.5" },
  { name: "Android", version: "14" },
];
const RELEASES = ["web@1.8.2", "web@1.8.1", "web@1.7.4"];
const ENVIRONMENTS = ["production", "staging"];

interface IssueRecord {
  summary: IssueSummary;
  culprit: string;
  events: ErrorEvent[];
}

function symbolicatedFrames(rng: Rng): StackFrame[] {
  const appCount = rng.int(2, 4);
  const frames: StackFrame[] = [];
  for (let i = 0; i < appCount; i++) {
    const file = rng.pick(APP_FILES);
    const fn = rng.pick(APP_FNS);
    const lineno = rng.int(12, 240);
    frames.push({
      function: fn,
      filename: file,
      lineno,
      colno: rng.int(3, 60),
      in_app: true,
      context_line: `    return ${fn}(${rng.pick(["item", "props", "data", "ctx"])});`,
    });
  }
  // Interleave a couple of framework frames at the bottom of the stack.
  frames.push({ ...rng.pick(VENDOR_FRAMES) });
  frames.push({ ...rng.pick(VENDOR_FRAMES) });
  return frames;
}

/** The original minified stack that symbolication maps back to source. */
function rawFrames(symbolicated: StackFrame[], rng: Rng): StackFrame[] {
  const bundle = `assets/index-${hexId(`bundle|${rng.int(0, 1e9)}`, 8)}.js`;
  const names = ["t", "e", "n", "r", "o", "i", "a", "s.default"];
  // Mirror the symbolicated frames' in_app flag so the app/system split (and the
  // default app-only view) is preserved when toggling to the raw stack.
  return symbolicated.map((f, idx) => ({
    function: names[idx % names.length],
    filename: bundle,
    lineno: 1,
    colno: rng.int(1200, 98000),
    in_app: f.in_app,
  }));
}

function buildEvent(
  issueSeed: string,
  index: number,
  tmpl: IssueTemplate,
  timestamp: number,
  sessionId: string | undefined,
): ErrorEvent {
  const rng = makeRng(`${issueSeed}|event|${index}`);
  const symbolicated = symbolicatedFrames(rng);
  const hasRaw = tmpl.level === "error" && rng.bool(0.65);
  const browser = rng.pick(BROWSERS);
  const os = rng.pick(OSES);
  const hasTrace = rng.bool(0.7);
  const traceId = hexId(`${issueSeed}|trace|${index}`, 32);
  const eventId = hexId(`${issueSeed}|evid|${index}`, 32);
  const url = rng.pick([
    "https://app.lazyobserve.io/checkout",
    "https://app.lazyobserve.io/orders/48213",
    "https://shop.example.com/cart",
  ]);

  return {
    event_id: eventId,
    timestamp,
    level: tmpl.level,
    exception: {
      type: tmpl.type,
      value: tmpl.value,
      stacktrace: { frames: symbolicated },
      raw_stacktrace: hasRaw ? { frames: rawFrames(symbolicated, rng) } : undefined,
    },
    contexts: {
      trace: hasTrace ? { trace_id: traceId, span_id: hexId(`${eventId}|span`, 16) } : undefined,
      browser,
      os,
    },
    session_id: sessionId,
    tags: {
      environment: rng.pick(ENVIRONMENTS),
      release: rng.pick(RELEASES),
      browser: `${browser.name} ${browser.version}`,
      os: `${os.name} ${os.version}`,
      url,
      transaction: rng.pick(APP_FILES).replace("src/", ""),
    },
    url,
    user: { id: `u_${hexId(`${issueSeed}|user|${index}`, 8)}` },
  };
}

function buildTrend(rng: Rng, count: number): number[] {
  const buckets = 14;
  const base = Math.max(1, Math.round(count / buckets));
  return Array.from({ length: buckets }, (_, i) => {
    const spike = i >= buckets - 3 ? rng.int(1, 3) : 1;
    return Math.max(0, Math.round(base * rng.next() * 2 * spike));
  });
}

function buildIssue(index: number): IssueRecord {
  const seed = `issue-${index}`;
  const rng = makeRng(seed);
  const tmpl = TEMPLATES[index % TEMPLATES.length];

  // Status distribution keeps every tab populated.
  const status: IssueStatus = index < 15 ? "open" : index < 20 ? "resolved" : "ignored";

  // Error-level issues link their most-recent event to a replay session so the
  // Issue -> Replay jump + timeline marker are consistent.
  const errSessions = errorSessionIds();
  const linkedSession =
    tmpl.level === "error" && errSessions.length > 0
      ? errSessions[index % errSessions.length]
      : undefined;

  const eventCount = rng.int(3, 8);
  // For a linked issue, anchor the most-recent event INSIDE the session window
  // (spread by issue index so multiple issues on one session don't overlap), so
  // the replay error marker always lands on the timeline within bounds.
  const meta = linkedSession ? sessionMeta(linkedSession) : undefined;
  const anchor = meta
    ? Math.round(meta.startedAtMs + meta.durationMs * (0.2 + (index % 5) * 0.15))
    : BASE_NOW - rng.int(5, 600) * 60_000;

  const events: ErrorEvent[] = [];
  for (let i = 0; i < eventCount; i++) {
    // The 0th event is the most-recent (linked) one; others spread into the past.
    // Only the linked event carries the session id, so every replay marker sits
    // inside its session's recording window.
    const ts =
      i === 0 ? anchor : anchor - rng.int(1, 72) * 3_600_000 - rng.int(0, 59) * 60_000;
    const sid = i === 0 ? linkedSession : undefined;
    events.push(buildEvent(seed, i, tmpl, ts, sid));
  }
  events.sort((a, b) => b.timestamp - a.timestamp);

  const lastSeen = events[0].timestamp;
  const firstSeen = events[events.length - 1].timestamp;
  const count = rng.int(eventCount * 6, eventCount * 400);
  const topApp = events[0].exception.stacktrace.frames.find((f) => f.in_app);
  const culprit = topApp
    ? `${topApp.function}(${topApp.filename})`
    : events[0].exception.type;

  const summary: IssueSummary = {
    id: `iss_${hexId(seed, 12)}`,
    fingerprint: hexId(`fp|${seed}`, 32),
    title: `${tmpl.type}: ${tmpl.value}`,
    level: tmpl.level,
    status,
    count,
    user_count: Math.max(1, Math.round(count * rng.next() * 0.6)),
    first_seen: firstSeen,
    last_seen: lastSeen,
    trend: buildTrend(rng, count),
  };

  return { summary, culprit, events };
}

// ── store ──────────────────────────────────────────────────────────────────
const ISSUES: IssueRecord[] = Array.from({ length: ISSUE_COUNT }, (_, i) => buildIssue(i));

function delay<T>(value: T, ms = 220): Promise<T> {
  return new Promise((resolve) => setTimeout(() => resolve(value), ms));
}

function projectEvent(e: ErrorEvent): IssueEventSummary {
  return {
    event_id: e.event_id,
    timestamp: e.timestamp,
    trace_id: e.contexts.trace?.trace_id,
    session_id: e.session_id,
  };
}

export function mockListIssues(params: IssueListParams): Promise<IssueListResult> {
  const q = (params.q ?? "").trim();
  if (q.includes("__error")) {
    return new Promise((_, reject) =>
      setTimeout(() => reject(new Error(`issue search failed: ${q}`)), 220),
    );
  }

  let rows = ISSUES.map((r) => r.summary).filter((s) => s.status === params.status);
  if (params.level) rows = rows.filter((s) => s.level === params.level);
  if (q && !q.includes("__empty")) {
    const needle = q.toLowerCase();
    rows = rows.filter((s) => s.title.toLowerCase().includes(needle));
  }
  if (q.includes("__empty")) rows = [];

  const sort = params.sort ?? "last_seen";
  rows = [...rows].sort((a, b) =>
    sort === "count" ? b.count - a.count : b.last_seen - a.last_seen,
  );

  const total = rows.length;
  const page = Math.max(1, params.page ?? 1);
  const start = (page - 1) * PAGE_SIZE;
  return delay({
    issues: rows.slice(start, start + PAGE_SIZE),
    total,
    page,
    page_size: PAGE_SIZE,
  });
}

export function mockGetIssue(id: string): Promise<IssueDetail> {
  const rec = ISSUES.find((r) => r.summary.id === id);
  if (!rec) return Promise.reject(new Error(`issue ${id} not found`));
  return delay({
    ...rec.summary,
    culprit: rec.culprit,
    events: rec.events.map(projectEvent),
  });
}

export function mockUpdateIssueStatus(id: string, status: IssueStatus): Promise<IssueSummary> {
  const rec = ISSUES.find((r) => r.summary.id === id);
  if (!rec) return Promise.reject(new Error(`issue ${id} not found`));
  rec.summary.status = status;
  return delay({ ...rec.summary }, 180);
}

export function mockGetIssueEvent(issueId: string, eventId: string): Promise<ErrorEvent> {
  const rec = ISSUES.find((r) => r.summary.id === issueId);
  const event = rec?.events.find((e) => e.event_id === eventId);
  if (!event) return Promise.reject(new Error(`event ${eventId} not found`));
  return delay(event, 160);
}

/** Error markers (Issue events) recorded during a given replay session. */
export interface SessionErrorMarker {
  time: number;
  issue_id: string;
  event_id: string;
  title: string;
  level: IssueLevel;
}

export function errorEventsForSession(sessionId: string): SessionErrorMarker[] {
  const markers: SessionErrorMarker[] = [];
  for (const rec of ISSUES) {
    for (const e of rec.events) {
      if (e.session_id === sessionId) {
        markers.push({
          time: e.timestamp,
          issue_id: rec.summary.id,
          event_id: e.event_id,
          title: rec.summary.title,
          level: rec.summary.level,
        });
      }
    }
  }
  return markers.sort((a, b) => a.time - b.time);
}
