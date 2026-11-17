import { hexId, makeRng } from "./mock-random";
import dayjs from "../dayjs.ts";

/**
 * Shared session identity for the offline mock (VITE_MOCK=1).
 *
 * A "session" is the unit that ties Replay and Issues together: a replay
 * recording and the error events captured during that same browser session
 * share a `session_id`. To keep the cross-links deterministic AND acyclic, the
 * session *identity* (id, window, whether it errored) lives here — with no
 * dependency on either domain. `mock-issues` reads it to place error events
 * inside a session's window; `mock-replay` reads it to build the session list
 * and segment timeline. Both agree because both derive everything from the id.
 */

/** Captured once at load so relative "started N ago" stays stable per session. */
const BASE_NOW = dayjs().valueOf();

const PAGE_URLS = [
  "https://app.lazyobserve.io/checkout",
  "https://app.lazyobserve.io/settings/profile",
  "https://app.lazyobserve.io/orders/48213",
  "https://shop.example.com/cart",
  "https://shop.example.com/product/sku-8842",
];

const SESSION_COUNT = 16;

export interface SessionSeedMeta {
  sessionId: string;
  /** Epoch ms of the first recorded segment. */
  startedAtMs: number;
  /** Total recording length in ms. */
  durationMs: number;
  segmentCount: number;
  totalBytes: number;
  hasError: boolean;
  pageUrl: string;
}

/** Deterministic metadata for a session id — pure, so any id resolves stably. */
export function sessionMeta(sessionId: string): SessionSeedMeta {
  const rng = makeRng(`meta|${sessionId}`);
  const durationMs = rng.int(20, 240) * 1000; // 20s .. 4min
  const agoMs = rng.int(3, 24 * 60) * 60_000; // 3min .. 24h ago
  const startedAtMs = BASE_NOW - agoMs;
  const segmentCount = Math.max(2, Math.round(durationMs / 1000 / 6)); // ~1 seg / 6s
  const totalBytes = segmentCount * rng.int(9_000, 45_000);
  return {
    sessionId,
    startedAtMs,
    durationMs,
    segmentCount,
    totalBytes,
    hasError: rng.bool(0.45),
    pageUrl: rng.pick(PAGE_URLS),
  };
}

/** The stable set of mock session ids, newest-started first. */
export const MOCK_SESSION_IDS: string[] = Array.from(
  { length: SESSION_COUNT },
  (_, i) => `sess_${hexId(`replay-session-${i}`, 24)}`,
);

/** Session ids flagged as having captured an error (used to link Issue events). */
export function errorSessionIds(): string[] {
  return MOCK_SESSION_IDS.filter((id) => sessionMeta(id).hasError);
}

/**
 * When (epoch ms) the error occurred within a session's recording window.
 * Both the Issue event timestamp and the Replay error marker derive from this,
 * so the marker always lands on the timeline at the right offset.
 */
export function sessionErrorTimeMs(sessionId: string): number {
  const m = sessionMeta(sessionId);
  return Math.round(m.startedAtMs + m.durationMs * 0.55);
}
