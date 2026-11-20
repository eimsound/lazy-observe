import type { TraceTarget } from "./types";
import { randomHex } from "./utils";

export interface TraceContext { trace_id: string; span_id: string }

export type SpanAttribute = string | number | boolean;

export interface RecordedSpan {
  context: TraceContext;
  parentSpanId?: string;
  name: string;
  kind: 1 | 3;
  startTimeUnixNano: string;
  endTimeUnixNano: string;
  attributes?: Record<string, SpanAttribute>;
  status: { code: 1 | 2; message?: string };
}

export function unixNanoNow(): string {
  const milliseconds = typeof performance !== "undefined" && Number.isFinite(performance.timeOrigin)
    ? performance.timeOrigin + performance.now()
    : Date.now();
  return BigInt(Math.floor(milliseconds * 1_000_000)).toString();
}

export function unixNanoAfter(start: string): string {
  const now = BigInt(unixNanoNow());
  const started = BigInt(start);
  return (now > started ? now : started + 1n).toString();
}

export function createTraceContext(parent?: TraceContext): TraceContext {
  return { trace_id: parent?.trace_id ?? randomHex(16), span_id: randomHex(8) };
}
export function traceparent(context: TraceContext): string { return `00-${context.trace_id}-${context.span_id}-01`; }
export function isTraceparent(value: string): boolean { return /^00-[0-9a-f]{32}-[0-9a-f]{16}-01$/.test(value); }
export function parseTraceparent(value: string): TraceContext | undefined {
  const match = value.match(/^00-([0-9a-f]{32})-([0-9a-f]{16})-0[01]$/);
  return match ? { trace_id: match[1], span_id: match[2] } : undefined;
}

function matchesStringAllowlist(url: URL, rule: string): boolean {
  try {
    const target = new URL(rule);
    if (url.origin !== target.origin) return false;
    if (target.pathname.endsWith("/")) return url.pathname.startsWith(target.pathname);
    return url.pathname === target.pathname || url.pathname.startsWith(`${target.pathname}/`);
  } catch {
    return false;
  }
}

export function shouldTrace(input: RequestInfo | URL, allowlist: TraceTarget[], origin?: string): boolean {
  const raw = typeof Request !== "undefined" && input instanceof Request ? input.url : String(input);
  let url: URL;
  try { url = new URL(raw, origin ?? (typeof location === "undefined" ? undefined : location.href)); } catch { return false; }
  const ownOrigin = origin ?? (typeof location === "undefined" ? undefined : location.origin);
  if (ownOrigin && url.origin === ownOrigin) return true;
  return allowlist.some((rule) => (
    typeof rule === "string"
      ? matchesStringAllowlist(url, rule)
      : (rule.lastIndex = 0, rule.test(url.href))
  ));
}

export class TraceWindow {
  private active: TraceContext[] = [];
  private callbacks: TraceContext[] = [];
  enter(context: TraceContext): () => void {
    this.active.push(context);
    return () => { const index = this.active.lastIndexOf(context); if (index >= 0) this.active.splice(index, 1); };
  }
  current(): TraceContext | undefined {
    const callback = this.currentCallback();
    if (callback) return callback;
    const context = this.active[this.active.length - 1];
    if (!context) return undefined;
    return this.active.every((candidate) => candidate.trace_id === context.trace_id) ? context : undefined;
  }
  currentCallback(): TraceContext | undefined { return this.callbacks[this.callbacks.length - 1]; }
  run<T>(context: TraceContext, callback: () => T): T {
    const leave = this.enter(context);
    this.callbacks.push(context);
    const leaveCallback = () => {
      const index = this.callbacks.lastIndexOf(context);
      if (index >= 0) this.callbacks.splice(index, 1);
    };
    try {
      const result = callback();
      leaveCallback();
      if (result instanceof Promise) return result.finally(leave) as T;
      leave();
      return result;
    } catch (error) {
      leaveCallback();
      leave();
      throw error;
    }
  }
}
