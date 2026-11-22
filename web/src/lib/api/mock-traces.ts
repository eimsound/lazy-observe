import { hexId, makeRng, type Rng } from "./mock-random";
import { mockLLMTraceMetadata } from "./mock-llm";
import dayjs from "../dayjs.ts";
import type {
  JaegerKeyValue,
  JaegerProcess,
  JaegerSpan,
  JaegerTrace,
  TraceSearchParams,
} from "./traces";

/**
 * Offline trace generator (VITE_MOCK=1). Produces realistic-looking Jaeger
 * traces — 5..30 spans across a small service graph, mixing serial and parallel
 * children, with occasional error paths. Everything is a PURE function of the
 * traceID, so getTrace(id) reproduces the exact structure a search row showed,
 * and a log line's trace_id resolves to a valid waterfall.
 */

interface ServiceDef {
  operations: string[];
  deps: string[];
  kind: "http" | "db" | "cache" | "queue";
}

const SERVICE_GRAPH: Record<string, ServiceDef> = {
  frontend: {
    kind: "http",
    operations: ["GET /", "GET /orders", "GET /checkout", "GET /profile"],
    deps: ["api-gateway"],
  },
  "api-gateway": {
    kind: "http",
    operations: [
      "HTTP GET /api/orders",
      "HTTP POST /api/checkout",
      "HTTP GET /api/user",
    ],
    deps: ["auth-service", "orders-service", "payments-service"],
  },
  "auth-service": {
    kind: "http",
    operations: ["verifyToken", "loadSession", "checkPermissions"],
    deps: ["redis", "postgres"],
  },
  "orders-service": {
    kind: "http",
    operations: ["listOrders", "getOrder", "createOrder", "reserveStock"],
    deps: ["postgres", "redis", "kafka"],
  },
  "payments-service": {
    kind: "http",
    operations: ["authorizeCharge", "capturePayment", "refund"],
    deps: ["postgres", "kafka"],
  },
  postgres: {
    kind: "db",
    operations: ["SELECT orders", "INSERT orders", "SELECT users", "UPDATE ledger"],
    deps: [],
  },
  redis: {
    kind: "cache",
    operations: ["GET session", "SETEX cache", "INCR rate_limit"],
    deps: [],
  },
  kafka: {
    kind: "queue",
    operations: ["publish order.created", "publish payment.captured"],
    deps: [],
  },
};

const ENTRY_SERVICES = ["frontend", "api-gateway"] as const;

export function mockServices(): string[] {
  return Object.keys(SERVICE_GRAPH);
}

export function mockOperations(service: string): string[] {
  return SERVICE_GRAPH[service]?.operations ?? [];
}

function kv(key: string, value: string | number | boolean): JaegerKeyValue {
  const type =
    typeof value === "boolean" ? "bool" : typeof value === "number" ? "int64" : "string";
  return { key, type, value };
}

function spanTags(def: ServiceDef, operationName: string, rng: Rng): JaegerKeyValue[] {
  const tags: JaegerKeyValue[] = [];
  switch (def.kind) {
    case "http": {
      const method = operationName.split(" ")[0].replace("HTTP", "").trim() || "GET";
      tags.push(kv("span.kind", rng.bool(0.5) ? "server" : "client"));
      tags.push(kv("http.method", /^[A-Z]+$/.test(method) ? method : "GET"));
      tags.push(kv("http.status_code", 200));
      tags.push(kv("component", "net/http"));
      break;
    }
    case "db":
      tags.push(kv("span.kind", "client"));
      tags.push(kv("db.system", "postgresql"));
      tags.push(kv("db.statement", `${operationName} WHERE tenant_id = $1`));
      tags.push(kv("peer.service", "postgres"));
      break;
    case "cache":
      tags.push(kv("span.kind", "client"));
      tags.push(kv("db.system", "redis"));
      tags.push(kv("db.statement", operationName));
      break;
    case "queue":
      tags.push(kv("span.kind", "producer"));
      tags.push(kv("messaging.system", "kafka"));
      tags.push(kv("messaging.destination", operationName.split(" ")[1] ?? "events"));
      break;
  }
  return tags;
}

/** Mark a span (and, if it's http, its status) as errored. */
function markError(span: JaegerSpan): void {
  if (span.tags.some((t) => t.key === "error")) return;
  span.tags.push(kv("error", true));
  span.tags.push(kv("otel.status_code", "ERROR"));
  const status = span.tags.find((t) => t.key === "http.status_code");
  if (status) status.value = 500;
  span.warnings = ["span reported an error"];
}

/**
 * Build a trace with span start times RELATIVE to the trace origin (first span
 * starts at 0). Absolute placement is applied later by the caller so the same
 * structure can appear at different wall-clock times.
 */
function buildRelativeTrace(traceID: string): JaegerTrace {
  const rng = makeRng(traceID);
  const maxSpans = rng.int(5, 30);
  const totalDuration = rng.int(40_000, 900_000); // µs (40ms .. 900ms)
  const spans: JaegerSpan[] = [];
  const used = new Set<string>();
  const parentOf = new Map<string, string | null>();
  let counter = 0;

  function newSpanId(): string {
    return hexId(`${traceID}-span-${counter++}`, 16);
  }

  function emit(
    service: string,
    operationName: string,
    parentId: string | null,
    start: number,
    duration: number,
    depth: number,
  ): void {
    if (spans.length >= maxSpans) return;
    const def = SERVICE_GRAPH[service];
    used.add(service);
    const spanID = newSpanId();
    const span: JaegerSpan = {
      traceID,
      spanID,
      operationName,
      startTime: start,
      duration: Math.max(1, duration),
      references: parentId
        ? [{ refType: "CHILD_OF", traceID, spanID: parentId }]
        : [],
      tags: spanTags(def, operationName, rng),
      processID: `p-${service}`,
      warnings: null,
    };
    spans.push(span);
    parentOf.set(spanID, parentId);

    if (def.deps.length === 0 || depth >= 4 || spans.length >= maxSpans) return;

    // Children occupy the inner 8%..95% of this span's duration.
    const regionStart = start + Math.round(duration * 0.08);
    const regionEnd = start + Math.round(duration * 0.95);
    const childCount = Math.min(
      rng.int(1, 3),
      Math.max(1, maxSpans - spans.length),
    );
    let cursor = regionStart;
    let prevStart = regionStart;

    for (let i = 0; i < childCount; i++) {
      if (spans.length >= maxSpans) break;
      const depService = rng.pick(def.deps);
      const depDef = SERVICE_GRAPH[depService];
      const op = rng.pick(depDef.operations);

      // ~40% of siblings (after the first) run in parallel with the previous.
      const parallel = i > 0 && rng.bool(0.4);
      const childStart = parallel ? prevStart : cursor;
      const room = regionEnd - childStart;
      if (room <= 2_000) break;

      const isLeaf = depDef.deps.length === 0;
      const childDur = isLeaf
        ? Math.min(room, rng.int(2_000, 45_000))
        : Math.max(4_000, Math.round(room * (rng.next() * 0.5 + 0.35)));
      const clamped = Math.min(childDur, room);

      emit(depService, op, spanID, childStart, clamped, depth + 1);
      prevStart = childStart;
      if (!parallel) cursor = childStart + clamped + rng.int(500, 6_000);
    }
  }

  const entry = rng.pick(ENTRY_SERVICES);
  emit(entry, rng.pick(SERVICE_GRAPH[entry].operations), null, 0, totalDuration, 0);

  // ~30% of traces contain an error: pick a leaf span, then propagate the error
  // up its ancestor chain so the waterfall shows a coherent failing path.
  if (rng.bool(0.3) && spans.length > 1) {
    const leaves = spans.filter(
      (s) => !spans.some((c) => c.references[0]?.spanID === s.spanID),
    );
    const target = rng.pick(leaves.length ? leaves : spans);
    let cur: string | null = target.spanID;
    const byId = new Map(spans.map((s) => [s.spanID, s]));
    while (cur) {
      const s = byId.get(cur);
      if (!s) break;
      markError(s);
      cur = parentOf.get(cur) ?? null;
    }
  }

  const processes: Record<string, JaegerProcess> = {};
  for (const service of used) {
    processes[`p-${service}`] = {
      serviceName: service,
      tags: [
        kv("hostname", `${service}-${(makeRng(service).int(1, 6))}`),
        kv("ip", `10.0.${makeRng(service).int(0, 4)}.${makeRng(service + traceID).int(2, 250)}`),
      ],
    };
  }

  return { traceID, spans, processes, warnings: null };
}

/** Shift every span so the earliest one starts at `baseMicros`. */
function shiftTrace(trace: JaegerTrace, baseMicros: number): JaegerTrace {
  const origin = Math.min(...trace.spans.map((s) => s.startTime));
  return {
    ...trace,
    spans: trace.spans.map((s) => ({
      ...s,
      startTime: baseMicros + (s.startTime - origin),
    })),
  };
}

function traceDuration(trace: JaegerTrace): number {
  const start = Math.min(...trace.spans.map((s) => s.startTime));
  const end = Math.max(...trace.spans.map((s) => s.startTime + s.duration));
  return end - start;
}

export function mockGetTrace(traceID: string): Promise<JaegerTrace> {
  const trace = buildRelativeTrace(traceID);
  decorateLLMTrace(trace);
  const nowMicros = dayjs().valueOf() * 1000;
  const mockControl = globalThis as {
    __loMockTraceAgeMsById?: Record<string, number>;
  };
  const overrideAgeMs = mockControl.__loMockTraceAgeMsById?.[traceID];
  const jitter =
    typeof overrideAgeMs === "number" && Number.isFinite(overrideAgeMs)
      ? Math.max(0, Math.round(overrideAgeMs * 1_000))
      : (makeRng(traceID).int(0, 3600) * 1_000_000) % 3_600_000_000;
  const base = nowMicros - traceDuration(trace) - jitter;
  return delay(shiftTrace(trace, base), 200);
}

function decorateLLMTrace(trace: JaegerTrace): void {
  const metadata = mockLLMTraceMetadata(trace.traceID);
  const span = trace.spans[0];
  if (!metadata || !span) return;
  span.operationName = `chat ${metadata.model}`;
  const process = trace.processes[span.processID];
  if (process) process.serviceName = metadata.service;
  span.tags.push(
    kv("gen_ai.operation.name", "chat"),
    kv("gen_ai.provider.name", metadata.provider),
    kv("gen_ai.system", metadata.provider),
    kv("gen_ai.request.model", metadata.model),
    kv("gen_ai.response.model", metadata.model),
    kv("gen_ai.usage.input_tokens", metadata.inputTokens),
    kv("gen_ai.usage.output_tokens", metadata.outputTokens),
  );
  if (metadata.captureContent) {
    span.tags.push(
      kv(
        "gen_ai.input.messages",
        JSON.stringify([{ role: "user", parts: [{ type: "text", content: "mock opt-in prompt" }] }]),
      ),
      kv(
        "gen_ai.output.messages",
        JSON.stringify([{ role: "assistant", parts: [{ type: "text", content: "mock completion" }] }]),
      ),
    );
  }
}

export function mockSearchTraces(params: TraceSearchParams): Promise<JaegerTrace[]> {
  const tags = params.tags?.trim() ?? "";
  if (tags.includes("__empty")) return delay([], 250);
  if (tags.includes("__error")) {
    return Promise.reject(new Error(`invalid tag filter: ${tags}`));
  }

  const onlyErrors = /error\s*=\s*true/i.test(tags);
  const count = Math.max(1, Math.min(params.limit, 20));
  const key = `${params.service}|${params.operation ?? ""}|${count}`;
  const window = Math.max(1, params.end - params.start);

  const traces: JaegerTrace[] = [];
  for (let i = 0; i < count; i++) {
    const traceID = hexId(`${key}#${i}`, 32);
    const trace = buildRelativeTrace(traceID);
    // Spread trace starts across the requested window, leaving room to finish.
    const slot = params.start + Math.round((window * (i + 0.5)) / count);
    const base = Math.min(slot, params.end - traceDuration(trace));
    traces.push(shiftTrace(trace, Math.max(params.start, base)));
  }

  const minDuration = parseMockDuration(params.minDuration);
  const durationFiltered = minDuration === null
    ? traces
    : traces.filter((trace) => traceDuration(trace) >= minDuration);
  const filtered = onlyErrors
    ? durationFiltered.filter((t) => t.spans.some((s) => s.tags.some((tag) => tag.key === "error")))
    : durationFiltered;
  filtered.sort(
    (a, b) =>
      Math.min(...b.spans.map((s) => s.startTime)) -
      Math.min(...a.spans.map((s) => s.startTime)),
  );
  return delay(filtered, 300);
}

function parseMockDuration(value: string | undefined): number | null {
  if (!value) return null;
  const match = /^(\d+(?:\.\d+)?)(us|µs|ms)$/.exec(value.trim());
  if (!match) return null;
  return Math.ceil(Number(match[1]) * (match[2] === "ms" ? 1_000 : 1));
}

function delay<T>(value: T, ms: number): Promise<T> {
  return new Promise((resolve) => setTimeout(() => resolve(value), ms));
}
