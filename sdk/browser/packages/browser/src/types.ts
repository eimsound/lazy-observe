import type { TraceContext } from "./trace";

export type VitalName = "LCP" | "CLS" | "INP" | "TTFB" | "FCP";
export type VitalRating = "good" | "needs-improvement" | "poor";
export type VitalDeviceType = "desktop" | "mobile" | "tablet" | "unknown";
export type VitalBrowserFamily = "chrome" | "edge" | "safari" | "firefox" | "opera" | "other";
export type VitalOSFamily = "windows" | "macos" | "ios" | "android" | "linux" | "chromeos" | "other";

export interface VitalDimensions {
  device_type?: VitalDeviceType;
  browser_family?: VitalBrowserFamily;
  os_family?: VitalOSFamily;
  country?: string;
}

export interface VitalsConfig {
  /** Optional explicit 2-letter ISO 3166-1 alpha-2 country code (e.g. "US", "CN"). */
  country?: string;
}

/** Safe Web Vital fields mirrored into the rrweb custom-event stream. */
export interface ReplayVitalSample {
  timestamp: number;
  name: VitalName;
  value: number;
  rating: VitalRating;
}

export interface StackFrame {
  function: string;
  filename: string;
  lineno?: number;
  colno?: number;
  in_app: boolean;
}

export interface ErrorEventPayload {
  event_id: string;
  timestamp: string;
  platform: "javascript";
  release?: string;
  environment?: string;
  level: "error" | "warning";
  message?: string;
  exception: {
    type: string;
    value: string;
    stacktrace: { frames: StackFrame[] };
  };
  tags?: Record<string, string>;
  contexts?: {
    browser?: { name: string; version?: string };
    os?: { name: string };
    trace?: { trace_id: string; span_id: string };
  };
  session_id?: string;
  url?: string;
}

export interface ReplayConfig {
  sampleRate?: number;
  onErrorFullCapture?: boolean;
  onErrorOnly?: boolean;
  bufferMs?: number;
  checkoutEveryNms?: number;
  flushIntervalMs?: number;
  maxSegmentBytes?: number;
  blockSelector?: string;
}

export interface DeliveryConfig {
  errorQueueCapacity?: number;
  traceQueueCapacity?: number;
  vitalQueueCapacity?: number;
  replayQueueCapacity?: number;
  diagnosticQueueCapacity?: number;
  maxAttempts?: number;
  retryInitialMs?: number;
  retryMaxMs?: number;
}

export interface BrowserDiagnostics {
  droppedTelemetry: number;
}

export type TraceTarget = string | RegExp;

export interface BrowserConfig {
  dsn: string;
  serviceName?: string;
  errors?: boolean;
  vitals?: boolean | VitalsConfig;
  /** Optional explicit 2-letter ISO 3166-1 alpha-2 country code attached to Web Vitals. */
  country?: string;
  fetchTracing?: boolean | { autoPatch?: boolean; allowlist?: TraceTarget[] };
  replay?: boolean | ReplayConfig;
  beforeSend?: (event: ErrorEventPayload) => ErrorEventPayload | null | undefined;
  release?: string;
  environment?: string;
  errorLimitPerMinute?: number;
  errorBatchSize?: number;
  errorFlushIntervalMs?: number;
  delivery?: DeliveryConfig;
}

export interface CaptureOptions {
  level?: "error" | "warning";
  tags?: Record<string, string>;
  trace?: TraceContext;
}

export interface StartSpanOptions {
  name: string;
}

export type Dispose = () => void;

export interface ActiveSpan {
  readonly name: string;
  readonly context: TraceContext;
}

export interface BrowserClient {
  readonly sessionId: string;
  startSpan<T>(options: StartSpanOptions, callback: (span: ActiveSpan) => T): T;
  captureException(error: unknown, options?: CaptureOptions): string | undefined;
  wrapFetch(fetchImpl?: typeof fetch): typeof fetch;
  setPage(page: string): void;
  setCountry(country: string | undefined): void;
  diagnostics(): BrowserDiagnostics;
  flush(): Promise<void>;
  close(): Promise<void>;
}
