import type {
  ReplayListParams,
  ReplayListResult,
  ReplayMarker,
  ReplaySessionDetail,
  ReplaySessionSummary,
  RRWebEvent,
} from "./replay";
import { makeRng } from "./mock-random";
import {
  MOCK_SESSION_IDS,
  sessionErrorTimeMs,
  sessionMeta,
} from "./mock-sessions";
import { errorEventsForSession } from "./mock-issues";

/**
 * Offline rrweb recording generator (VITE_MOCK=1).
 *
 * We hand-author valid rrweb events so rrweb's Replayer can actually play them:
 * a Meta + FullSnapshot (a small, self-contained DOM) in segment 0, then
 * IncrementalSnapshots (mouse movement, text mutations, and — at the error
 * moment — a click plus an injected error banner) spread across the session
 * window. Segments are independent slices loaded on demand and concatenated in
 * order by the player wrapper.
 *
 * rrweb numeric enums used below (see @rrweb/types):
 *   EventType:        Meta=4, FullSnapshot=2, IncrementalSnapshot=3
 *   IncrementalSource: Mutation=0, MouseMove=1, MouseInteraction=2
 *   MouseInteractions: Click=2
 *   NodeType:          Document=0, DocumentType=1, Element=2, Text=3
 * Objects are built plainly and cast to the rrweb event union at the boundary —
 * the standard approach for hand-written rrweb fixtures.
 *
 * NOTE: the CSS/markup below is *recorded-page data* (what the user's site looked
 * like), not lazy-observe UI. Its colors are part of the fixture, analogous to a
 * screenshot; the player chrome around it is themed in globals.css (.lo-rrweb).
 */

// Stable node ids referenced by later incremental mutations.
const ID = {
  document: 1,
  doctype: 2,
  html: 3,
  head: 4,
  style: 5,
  styleText: 6,
  body: 7,
  wrapper: 8,
  header: 9,
  headerText: 10,
  main: 11,
  heading: 12,
  headingText: 13,
  paragraph: 14,
  paragraphText: 15,
  button: 16,
  buttonText: 17,
};

const PAGE_CSS = `
  * { box-sizing: border-box; }
  body { margin: 0; font-family: -apple-system, Segoe UI, Roboto, sans-serif; background: #f4f5f7; color: #1f2430; }
  .wrap { max-width: 960px; margin: 0 auto; padding: 24px; }
  .bar { display: flex; align-items: center; gap: 12px; padding: 14px 18px; background: #ffffff; border: 1px solid #e4e6eb; border-radius: 10px; font-weight: 600; }
  .dot { width: 12px; height: 12px; border-radius: 50%; background: #4f46e5; }
  .card { margin-top: 20px; padding: 22px; background: #ffffff; border: 1px solid #e4e6eb; border-radius: 10px; }
  h1 { font-size: 22px; margin: 0 0 8px; }
  p { color: #5a6072; margin: 0 0 18px; line-height: 1.5; }
  button { padding: 10px 18px; border: none; border-radius: 8px; background: #4f46e5; color: #fff; font-size: 14px; cursor: pointer; }
  .err { margin-top: 18px; padding: 12px 16px; border-radius: 8px; background: #fdecec; color: #b42318; border: 1px solid #f5c2c0; font-size: 14px; }
`;

/** Serialized FullSnapshot document tree (plain object; cast at the boundary). */
function fullSnapshotNode(): unknown {
  const el = (
    tagName: string,
    id: number,
    attributes: Record<string, string>,
    childNodes: unknown[],
  ) => ({ type: 2, tagName, attributes, childNodes, id });
  const text = (textContent: string, id: number, isStyle?: boolean) => ({
    type: 3,
    textContent,
    id,
    ...(isStyle ? { isStyle: true } : {}),
  });

  return {
    type: 0,
    id: ID.document,
    childNodes: [
      { type: 1, name: "html", publicId: "", systemId: "", id: ID.doctype },
      el("html", ID.html, { lang: "en" }, [
        el("head", ID.head, {}, [
          el("style", ID.style, { type: "text/css" }, [text(PAGE_CSS, ID.styleText, true)]),
        ]),
        el("body", ID.body, {}, [
          el("div", ID.wrapper, { class: "wrap" }, [
            el("div", ID.header, { class: "bar" }, [
              { type: 2, tagName: "span", attributes: { class: "dot" }, childNodes: [], id: 100 },
              text("lazy-observe demo app", ID.headerText),
            ]),
            el("div", ID.main, { class: "card" }, [
              el("h1", ID.heading, {}, [text("Welcome back", ID.headingText)]),
              el("p", ID.paragraph, {}, [
                text("Loading your data…", ID.paragraphText),
              ]),
              el("button", ID.button, { type: "button" }, [text("Retry", ID.buttonText)]),
            ]),
          ]),
        ]),
      ]),
    ],
  };
}

function errorBannerAdd(): unknown {
  return {
    parentId: ID.main,
    nextId: null,
    node: {
      type: 2,
      tagName: "div",
      attributes: { class: "err" },
      childNodes: [
        { type: 3, textContent: "⚠ Uncaught TypeError: Cannot read properties of undefined", id: 9001 },
      ],
      id: 9000,
    },
  };
}

/** Build one segment's events. Segment 0 carries Meta + FullSnapshot. */
export function mockReplaySegment(sessionId: string, seq: number): Promise<RRWebEvent[]> {
  const meta = sessionMeta(sessionId);
  const segMs = meta.durationMs / meta.segmentCount;
  const segStart = Math.round(meta.startedAtMs + seq * segMs);
  const rng = makeRng(`${sessionId}|seg|${seq}`);
  const events: unknown[] = [];

  if (seq === 0) {
    events.push({
      type: 4,
      data: { href: meta.pageUrl, width: 1280, height: 720 },
      timestamp: segStart,
    });
    events.push({
      type: 2,
      data: { node: fullSnapshotNode(), initialOffset: { top: 0, left: 0 } },
      timestamp: segStart + 1,
    });
  }

  // Mouse movement sampled across the segment (timeOffset ramps up to 0).
  const moveTime = segStart + Math.floor(segMs * 0.5);
  const positions = Array.from({ length: 10 }, (_, i) => ({
    x: rng.int(40, 1200),
    y: rng.int(60, 680),
    id: ID.body,
    timeOffset: -Math.round(segMs * 0.5 * ((9 - i) / 9)),
  }));
  events.push({ type: 3, data: { source: 1, positions }, timestamp: moveTime });

  // Text mutation so the recorded DOM visibly changes over time.
  events.push({
    type: 3,
    data: {
      source: 0,
      texts: [{ id: ID.paragraphText, value: `Session activity — segment ${seq + 1}` }],
      attributes: [],
      removes: [],
      adds: [],
    },
    timestamp: segStart + Math.floor(segMs * 0.7),
  });

  // If the error falls in this segment, show a click + an injected error banner.
  if (meta.hasError) {
    const errTime = sessionErrorTimeMs(sessionId);
    if (errTime >= segStart && errTime < segStart + segMs) {
      events.push({
        type: 3,
        data: { source: 2, type: 2, id: ID.button, x: 220, y: 360 },
        timestamp: Math.max(segStart + 2, errTime - 200),
      });
      events.push({
        type: 3,
        data: { source: 0, texts: [], attributes: [], removes: [], adds: [errorBannerAdd()] },
        timestamp: errTime,
      });
    }
  }

  events.sort((a, b) => (a as { timestamp: number }).timestamp - (b as { timestamp: number }).timestamp);
  // Small per-segment latency so on-demand loading is observable, but low enough
  // that even a many-segment session assembles quickly.
  return new Promise((resolve) =>
    setTimeout(() => resolve(events as unknown as RRWebEvent[]), 40),
  );
}

function toSummary(sessionId: string): ReplaySessionSummary {
  const m = sessionMeta(sessionId);
  return {
    session_id: m.sessionId,
    started_at: m.startedAtMs,
    last_seen_at: m.startedAtMs + m.durationMs,
    segment_count: m.segmentCount,
    total_bytes: m.totalBytes,
    has_error: m.hasError,
    page_url: m.pageUrl,
  };
}

const PAGE_SIZE = 12;

function delay<T>(value: T, ms = 220): Promise<T> {
  return new Promise((resolve) => setTimeout(() => resolve(value), ms));
}

export function mockListReplaySessions(params: ReplayListParams): Promise<ReplayListResult> {
  let rows = MOCK_SESSION_IDS.map(toSummary);
  if (params.has_error !== undefined) {
    rows = rows.filter((s) => s.has_error === params.has_error);
  }
  rows.sort((a, b) => b.started_at - a.started_at);

  const total = rows.length;
  const page = Math.max(1, params.page ?? 1);
  const start = (page - 1) * PAGE_SIZE;
  return delay({
    sessions: rows.slice(start, start + PAGE_SIZE),
    total,
    page,
    page_size: PAGE_SIZE,
  });
}

export function mockGetReplaySession(id: string): Promise<ReplaySessionDetail> {
  const m = sessionMeta(id);
  const segMs = m.durationMs / m.segmentCount;
  const segments = Array.from({ length: m.segmentCount }, (_, seq) => ({
    seq,
    started_at: Math.round(m.startedAtMs + seq * segMs),
    event_count: seq === 0 ? 5 : 3,
    size_bytes: Math.round(m.totalBytes / m.segmentCount),
  }));
  const errors = errorEventsForSession(id).map((e) => ({
    type: "error" as const,
    time: e.time,
    issue_id: e.issue_id,
    event_id: e.event_id,
    title: e.title,
    level: e.level,
  }));
  const markers: ReplayMarker[] = [
    ...errors,
    {
      type: "http",
      time: Math.round(m.startedAtMs + m.durationMs * 0.35),
      method: "GET",
      path: "/api/cart",
      status: 200,
      duration_ms: 86.4,
    },
    {
      type: "http",
      time: Math.round(m.startedAtMs + m.durationMs * 0.62),
      method: "POST",
      path: "/api/orders",
      status: m.hasError ? 503 : 201,
      duration_ms: m.hasError ? 642.8 : 128.2,
    },
    {
      type: "vital",
      time: Math.round(m.startedAtMs + m.durationMs * 0.2),
      name: "LCP",
      value: 2180.5,
      rating: "good",
    },
  ];
  markers.sort((a, b) => a.time - b.time);
  return delay({
    ...toSummary(id),
    segments,
    markers,
  });
}
