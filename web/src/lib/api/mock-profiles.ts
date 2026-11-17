import { hexId, makeRng, type Rng } from "./mock-random";
import type {
  FlameNode,
  ProfileQuery,
  ProfileRecord,
  ProfileType,
} from "./profiles";

/**
 * Offline continuous-profiling generator (VITE_MOCK=1). Everything is a PURE
 * function of (service, type) so a flamegraph is stable across reloads and can
 * be verified by hand: the root value is 10000 and the five top-level frames
 * take fixed, round shares (see LEVEL1). Deeper frames are expanded with a
 * seeded PRNG. Guarantees for the acceptance test: >= 4 levels deep, >= 60
 * nodes, and a searchable signature frame (SIGNATURE) present in every tree.
 */

const ROOT_TOTAL = 10000;

/** The searchable "feature" frame the AC greps for. */
export const SIGNATURE_FRAME =
  "github.com/lazy-observe/profiler.collectFlameSamples";

const SERVICES = [
  "lazy-observe-gateway",
  "query-service",
  "ingest-service",
  "store-service",
  "frontend",
];

/** Fixed top level: [frame, share] — shares sum to 1.0 for easy verification. */
const LEVEL1: Array<[string, number]> = [
  ["net/http.(*conn).serve", 0.46],
  ["github.com/lazy-observe/query/promql.(*Engine).exec", 0.24],
  ["github.com/lazy-observe/ingest/pipeline.(*Batcher).run", 0.14],
  ["runtime.gcBgMarkWorker", 0.11],
  ["runtime.mcall", 0.05],
];

const APP_POOL = [
  "github.com/lazy-observe/query/promql.(*Evaluator).evalExpr",
  "github.com/lazy-observe/query/promql.(*parser).parseExpr",
  "github.com/lazy-observe/store/tsdb.(*Engine).Select",
  "github.com/lazy-observe/store/tsdb.(*blockReader).readSeries",
  "github.com/lazy-observe/store/wal.(*Writer).Append",
  "github.com/lazy-observe/ingest/parser.(*PromParser).Next",
  "github.com/lazy-observe/api/handler.(*Router).dispatch",
  "github.com/lazy-observe/api/handler.writeResponse",
  "github.com/lazy-observe/agent/sampler.(*Profiler).record",
];

const STD_POOL = [
  "encoding/json.Marshal",
  "encoding/json.(*decodeState).object",
  "bufio.(*Reader).ReadSlice",
  "compress/gzip.(*Writer).Write",
  "sync.(*Mutex).Lock",
  "net.(*netFD).Read",
  "sort.Slice",
  "strconv.ParseFloat",
];

const RUNTIME_POOL = [
  "runtime.mallocgc",
  "runtime.mapassign_faststr",
  "runtime.scanobject",
  "runtime.memmove",
  "runtime.growslice",
  "runtime.gcDrain",
];

const MAX_DEPTH = 6;
const MIN_LEAF = 24; // stop splitting below this many samples
const MAX_NODES = 360; // safety cap; hitting it marks a subtree truncated

interface BuildState {
  rng: Rng;
  count: number;
}

/** Pick `k` distinct frames from a pool, seeded, avoiding names already used. */
function pickFrames(state: BuildState, pool: string[], k: number, used: Set<string>): string[] {
  const out: string[] = [];
  let guard = 0;
  while (out.length < k && guard < 40) {
    guard++;
    const f = state.rng.pick(pool);
    if (!used.has(f)) {
      used.add(f);
      out.push(f);
    }
  }
  return out;
}

/** Expand a node's subtree in place. Returns nothing; mutates `node.children`. */
function expand(node: FlameNode, depth: number, state: BuildState): void {
  if (depth >= MAX_DEPTH || node.value < MIN_LEAF) return;
  if (state.count >= MAX_NODES) {
    node.truncated = true;
    return;
  }

  // Deeper frames skew toward runtime/stdlib leaves.
  const runtimeBias = Math.min(0.7, 0.15 + depth * 0.12);
  const childCount = state.rng.int(1, depth <= 1 ? 3 : 2);
  const used = new Set<string>();
  const frames: string[] = [];
  for (let i = 0; i < childCount; i++) {
    const roll = state.rng.next();
    const pool =
      roll < runtimeBias ? RUNTIME_POOL : roll < runtimeBias + 0.3 ? STD_POOL : APP_POOL;
    frames.push(...pickFrames(state, pool, 1, used));
  }

  // Children consume 60%..88% of this frame's value; the rest is self-time.
  const childBudget = Math.floor(node.value * (0.6 + state.rng.next() * 0.28));
  const weights = frames.map(() => 0.4 + state.rng.next());
  const wSum = weights.reduce((a, b) => a + b, 0);

  const children: FlameNode[] = [];
  for (let i = 0; i < frames.length; i++) {
    if (state.count >= MAX_NODES) {
      node.truncated = true;
      break;
    }
    const value = Math.max(1, Math.round((weights[i] / wSum) * childBudget));
    const child: FlameNode = { name: frames[i], value };
    state.count++;
    children.push(child);
    expand(child, depth + 1, state);
  }
  if (children.length) node.children = children;
}

/** Build a deterministic flamegraph for (service, type). */
function buildTree(service: string, type: ProfileType): FlameNode {
  const state: BuildState = { rng: makeRng(`profile|${service}|${type}`), count: 1 };
  const root: FlameNode = {
    name: `${service} (${type})`,
    value: ROOT_TOTAL,
    children: [],
  };

  const level1: FlameNode[] = LEVEL1.map(([name, share]) => {
    const child: FlameNode = { name, value: Math.round(ROOT_TOTAL * share) };
    state.count++;
    return child;
  });
  root.children = level1;

  // Expand each top-level subtree.
  for (const child of level1) expand(child, 1, state);

  // Inject the searchable signature frame under the promql engine subtree so it
  // sits at depth 2 with its own small subtree — present for every service/type.
  const promql = level1.find((c) => c.name.includes("promql"));
  if (promql) {
    const sig: FlameNode = {
      name: SIGNATURE_FRAME,
      value: Math.max(MIN_LEAF + 40, Math.round(promql.value * 0.22)),
      children: [
        { name: "runtime.mallocgc", value: Math.round(promql.value * 0.06) },
        { name: "encoding/json.Marshal", value: Math.round(promql.value * 0.05) },
      ],
    };
    state.count += 3;
    promql.children = [sig, ...(promql.children ?? [])];
  }

  // Deterministically clip one deep, hot leaf so the "truncated" banner is
  // exercised (a real backend clips oversized subtrees for blob size).
  markDeepestTruncated(root);

  return root;
}

/** Flag the largest leaf at the deepest level as clipped (truncated subtree). */
function markDeepestTruncated(root: FlameNode): void {
  let best: { node: FlameNode; depth: number } | null = null;
  const walk = (node: FlameNode, depth: number) => {
    const children = node.children;
    if (!children || children.length === 0) {
      if (!best || depth > best.depth || (depth === best.depth && node.value > best.node.value)) {
        best = { node, depth };
      }
      return;
    }
    children.forEach((c) => walk(c, depth + 1));
  };
  walk(root, 0);
  if (best) (best as { node: FlameNode }).node.truncated = true;
}

export function mockProfileServices(): Promise<string[]> {
  return delay([...SERVICES], 120);
}

export function mockFlamegraph(q: ProfileQuery): Promise<FlameNode> {
  return delay(buildTree(q.service, q.type), 200);
}

export function mockListProfiles(q: ProfileQuery): Promise<ProfileRecord[]> {
  const windowSec = Math.max(60, q.end - q.start);
  const rng = makeRng(`profiles|${q.service}|${q.type}|${Math.floor(q.start / 60)}`);
  const count = Math.min(14, Math.max(4, Math.floor(windowSec / 300) + 3));
  const records: ProfileRecord[] = [];
  for (let i = 0; i < count; i++) {
    // Spread capture windows across the requested range (newest first later).
    const start = Math.round(q.start + (windowSec * (i + 0.5)) / count);
    records.push({
      id: hexId(`${q.service}|${q.type}|${q.start}|${i}`, 24),
      service: q.service,
      type: q.type,
      startMs: start * 1000,
      endMs: (start + 15) * 1000,
      size: rng.int(38, 420) * 1024,
      ...(i % 3 === 0
        ? { traceId: hexId(`${q.service}|${q.type}|${q.start}|trace|${i}`, 32) }
        : {}),
    });
  }
  records.sort((a, b) => b.startMs - a.startMs);
  return delay(records, 160);
}

/** A tiny self-contained "download" so the raw link works offline. */
function delay<T>(value: T, ms: number): Promise<T> {
  return new Promise((resolve) => setTimeout(() => resolve(value), ms));
}
