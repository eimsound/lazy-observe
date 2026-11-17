import type { IssueLevel } from "./issue-contract";
import type { ReplaySegmentRef, ReplaySessionSummary } from "./replay";

export type ReplayMarkerType = "error" | "http" | "vital";
export type ReplayVitalName = "LCP" | "CLS" | "INP" | "TTFB" | "FCP";
export type ReplayVitalRating = "good" | "needs-improvement" | "poor";

interface ReplayMarkerBase {
  type: ReplayMarkerType;
  /** Epoch milliseconds at the original browser observation time. */
  time: number;
}

export interface ReplayErrorMarker extends ReplayMarkerBase {
  type: "error";
  issue_id?: string;
  event_id?: string;
  title: string;
  level: IssueLevel;
}

export interface ReplayHTTPMarker extends ReplayMarkerBase {
  type: "http";
  method: string;
  /** Path only. The SDK and gateway both remove origin, query and fragment. */
  path: string;
  /** 0 represents a network failure or opaque response. */
  status: number;
  duration_ms: number;
}

export interface ReplayVitalMarker extends ReplayMarkerBase {
  type: "vital";
  name: ReplayVitalName;
  value: number;
  rating: ReplayVitalRating;
}

export type ReplayMarker = ReplayErrorMarker | ReplayHTTPMarker | ReplayVitalMarker;

export interface ReplaySessionDetail extends ReplaySessionSummary {
  segments: ReplaySegmentRef[];
  /** All structured rrweb custom-event markers in chronological order. */
  markers: ReplayMarker[];
}

function record(value: unknown, label: string): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new TypeError(`${label} must be an object`);
  }
  return value as Record<string, unknown>;
}

function text(value: unknown, label: string, optional = false): string | undefined {
  if (optional && value === undefined) return undefined;
  if (typeof value !== "string") throw new TypeError(`${label} must be a string`);
  return value;
}

function number(value: unknown, label: string, integer = false): number {
  if (typeof value !== "number" || !Number.isFinite(value) || (integer && !Number.isInteger(value))) {
    throw new TypeError(`${label} must be a finite ${integer ? "integer" : "number"}`);
  }
  return value;
}

function boolean(value: unknown, label: string): boolean {
  if (typeof value !== "boolean") throw new TypeError(`${label} must be a boolean`);
  return value;
}

export function normalizeReplayMarker(value: unknown): ReplayMarker {
  const marker = record(value, "Replay marker");
  const time = number(marker.time, "Replay marker time", true);
  if (time <= 0) throw new TypeError("Replay marker time must be positive epoch milliseconds");

  if (marker.type === "error") {
    const level = text(marker.level, "Replay error level") as IssueLevel;
    if (!(["error", "warning", "info"] as const).includes(level)) {
      throw new TypeError("Replay error level is invalid");
    }
    return {
      type: "error",
      time,
      issue_id: text(marker.issue_id, "Replay issue_id", true),
      event_id: text(marker.event_id, "Replay event_id", true),
      title: text(marker.title, "Replay error title")!,
      level,
    };
  }

  if (marker.type === "http") {
    const method = text(marker.method, "Replay HTTP method")!;
    const path = text(marker.path, "Replay HTTP path")!;
    const status = number(marker.status, "Replay HTTP status", true);
    const duration = number(marker.duration_ms, "Replay HTTP duration");
    if (!/^[A-Z0-9!#$%&'*+.^_`|~-]{1,32}$/.test(method)) {
      throw new TypeError("Replay HTTP method is invalid");
    }
    if (!path.startsWith("/") || path.includes("?") || path.includes("#")) {
      throw new TypeError("Replay HTTP path must not contain a URL query or fragment");
    }
    if (status < 0 || status > 599 || duration < 0) {
      throw new TypeError("Replay HTTP status or duration is invalid");
    }
    return { type: "http", time, method, path, status, duration_ms: duration };
  }

  if (marker.type === "vital") {
    const name = text(marker.name, "Replay Web Vital name") as ReplayVitalName;
    const rating = text(marker.rating, "Replay Web Vital rating") as ReplayVitalRating;
    const valueNumber = number(marker.value, "Replay Web Vital value");
    if (!(["LCP", "CLS", "INP", "TTFB", "FCP"] as const).includes(name)) {
      throw new TypeError("Replay Web Vital name is invalid");
    }
    if (!(["good", "needs-improvement", "poor"] as const).includes(rating) || valueNumber < 0) {
      throw new TypeError("Replay Web Vital rating or value is invalid");
    }
    return { type: "vital", time, name, value: valueNumber, rating };
  }

  throw new TypeError("Replay marker type is invalid");
}

export function normalizeReplaySessionDetail(value: unknown): ReplaySessionDetail {
  const detail = record(value, "Replay session detail");
  if (!Array.isArray(detail.segments)) throw new TypeError("Replay session detail must contain segments");
  if (!Array.isArray(detail.markers)) throw new TypeError("Replay session detail must contain markers");

  const segments = detail.segments.map((value, index) => {
    const segment = record(value, `Replay segment ${index}`);
    const normalized = {
      seq: number(segment.seq, "Replay segment seq", true),
      started_at: number(segment.started_at, "Replay segment started_at", true),
      event_count: number(segment.event_count, "Replay segment event_count", true),
      size_bytes: number(segment.size_bytes, "Replay segment size_bytes", true),
    };
    if (Object.values(normalized).some((field) => field < 0)) {
      throw new TypeError("Replay segment fields must be non-negative");
    }
    return normalized;
  });
  const markers = detail.markers.map(normalizeReplayMarker);

  const result: ReplaySessionDetail = {
    session_id: text(detail.session_id, "Replay session_id")!,
    started_at: number(detail.started_at, "Replay started_at", true),
    last_seen_at: number(detail.last_seen_at, "Replay last_seen_at", true),
    segment_count: number(detail.segment_count, "Replay segment_count", true),
    total_bytes: number(detail.total_bytes, "Replay total_bytes", true),
    has_error: boolean(detail.has_error, "Replay has_error"),
    page_url: text(detail.page_url, "Replay page_url")!,
    segments,
    markers,
  };
  const release = text(detail.release, "Replay release", true);
  if (release !== undefined) result.release = release;
  if (result.started_at <= 0 || result.last_seen_at < result.started_at || result.segment_count < 0 || result.total_bytes < 0) {
    throw new TypeError("Replay session metadata is invalid");
  }
  return result;
}
