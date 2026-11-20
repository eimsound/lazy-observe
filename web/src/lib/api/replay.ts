import type { eventWithTime } from "@rrweb/types";
import { apiGet, IS_MOCK, API_ROOT } from "./client";
import {
  normalizeReplaySessionDetail,
  type ReplaySessionDetail,
} from "./replay-contract";
import dayjs from "../dayjs.ts";

// Mock data is only for VITE_MOCK builds; load it lazily so it never ships with real chunks.
const loadMockReplay = () => import("./mock-replay");

/**
 * Session Replay API. Sessions are rrweb recordings split into ordered,
 * gzip-compressed segments. Endpoints under /api:
 *   GET /api/replay/sessions?has_error=&start=&end=&page=   -> session list
 *   GET /api/replay/sessions/:id                            -> metadata + segments
 *   GET /api/replay/sessions/:id/segments/:seq              -> gzip'd rrweb events
 * The segment endpoint sets Content-Encoding: gzip, so the browser transparently
 * decompresses and we just parse the JSON array. VITE_MOCK generates valid
 * rrweb events (full snapshot + incremental) offline.
 */

/** A single rrweb event (re-exported so components don't import @rrweb/types). */
export type RRWebEvent = eventWithTime;

export interface ReplaySessionSummary {
  session_id: string;
  /** Epoch ms of the first recorded segment. */
  started_at: number;
  /** Epoch ms of the last recorded activity. */
  last_seen_at: number;
  segment_count: number;
  total_bytes: number;
  has_error: boolean;
  release?: string;
  page_url: string;
}

export interface ReplaySegmentRef {
  seq: number;
  /** Epoch ms of the segment's first event. */
  started_at: number;
  event_count: number;
  size_bytes: number;
}

export type {
  ReplayErrorMarker,
  ReplayHTTPMarker,
  ReplayMarker,
  ReplayMarkerType,
  ReplaySessionDetail,
  ReplayVitalMarker,
  ReplayVitalName,
  ReplayVitalRating,
} from "./replay-contract";

export interface ReplayListParams {
  has_error?: boolean;
  start?: number;
  end?: number;
  page?: number;
}

export interface ReplayListResult {
  sessions: ReplaySessionSummary[];
  total: number;
  page: number;
  page_size: number;
}

export async function listReplaySessions(
  params: ReplayListParams,
  signal?: AbortSignal,
  project?: string,
): Promise<ReplayListResult> {
  if (IS_MOCK) return (await loadMockReplay()).mockListReplaySessions(params);
  return apiGet<ReplayListResult>("/replay/sessions", {
    base: API_ROOT,
    params: {
      has_error: params.has_error === undefined ? undefined : String(params.has_error),
      start: params.start === undefined ? undefined : dayjs(params.start).toISOString(),
      end: params.end === undefined ? undefined : dayjs(params.end).toISOString(),
      page: params.page,
    },
    signal,
    project,
  });
}

export async function getReplaySession(
  id: string,
  signal?: AbortSignal,
  project?: string,
): Promise<ReplaySessionDetail> {
  if (IS_MOCK) return (await loadMockReplay()).mockGetReplaySession(id);
  const response = await apiGet<unknown>(`/replay/sessions/${encodeURIComponent(id)}`, {
    base: API_ROOT,
    signal,
    project,
  });
  return normalizeReplaySessionDetail(response);
}

/**
 * Fetch one segment's rrweb events. In real mode the response is gzip'd; the
 * browser decompresses it transparently (Content-Encoding: gzip) so a plain
 * JSON parse yields the event array.
 */
export async function getReplaySegment(
  id: string,
  seq: number,
  signal?: AbortSignal,
  project?: string,
): Promise<RRWebEvent[]> {
  if (IS_MOCK) return (await loadMockReplay()).mockReplaySegment(id, seq);
  return apiGet<RRWebEvent[]>(
    `/replay/sessions/${encodeURIComponent(id)}/segments/${seq}`,
    { base: API_ROOT, signal, project },
  );
}
