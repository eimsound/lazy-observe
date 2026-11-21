import { Client } from "./client";
import type { ActiveSpan, BrowserClient, BrowserConfig, CaptureOptions, StartSpanOptions } from "./types";

let current: BrowserClient | undefined;

export function init(config: BrowserConfig): BrowserClient {
  const previous = current;
  current = undefined;
  if (previous) void previous.close();
  const client: BrowserClient = new Client(config, () => {
    if (current === client) current = undefined;
  }).start();
  current = client;
  return client;
}

export function captureException(error: unknown, options?: CaptureOptions): string | undefined {
  return current?.captureException(error, options);
}

export function startSpan<T>(options: StartSpanOptions, callback: (span: ActiveSpan) => T): T {
  if (!current) throw new Error("Call init() before startSpan()");
  return current.startSpan(options, callback);
}

export function setPage(page: string): void { current?.setPage(page); }

export function setCountry(country: string | undefined): void { current?.setCountry(country); }

export function wrapFetch(fetchImpl?: typeof fetch): typeof fetch {
  if (!current) throw new Error("Call init() before wrapFetch()");
  return current.wrapFetch(fetchImpl);
}

export { parseDsn, redactDsn, type ParsedDsn, type ParseDsnOptions } from "./dsn";
export { MAX_ERROR_EVENT_BYTES, sanitizeErrorEvent, sanitizeUrl } from "./sanitize";
export { parseStack, isInApp } from "./stack";
export { MinuteRateLimiter } from "./rate-limit";
export { ReplayRingBuffer } from "./replay-buffer";
export { createTraceContext, isTraceparent, parseTraceparent, traceparent } from "./trace";
export type { TraceContext } from "./trace";
export {
  collectVitalDimensions,
  detectBrowserFamily,
  detectDeviceType,
  detectOSFamily,
  normalizeCountryCode,
  type VitalDimensionEnv,
} from "./vitals-dimensions";
export type {
  ActiveSpan,
  BrowserClient,
  BrowserConfig,
  BrowserDiagnostics,
  CaptureOptions,
  DeliveryConfig,
  Dispose,
  ErrorEventPayload,
  ReplayConfig,
  StackFrame,
  StartSpanOptions,
  TraceTarget,
  VitalBrowserFamily,
  VitalDeviceType,
  VitalDimensions,
  VitalName,
  VitalOSFamily,
  VitalsConfig,
} from "./types";
