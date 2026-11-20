import { ApiError } from "./client";
import { mockQueryRange } from "./mock";
import { mockQueryLogs } from "./mock-logs";
import { seriesLabel } from "./metrics";
import i18n from "@/lib/i18n";
import { rangeById, resolveRange } from "./ranges";
import type { TimeSeries } from "./types";
import type {
  AiSignal,
  AiSettings,
  AiSettingsUpdate,
  AiStatus,
  AiTestResult,
  AssistantAnswer,
  AssistantQueryRequest,
  AssistantResult,
  AssistantTimeRange,
  IssueAiAnalysis,
  ProbableCause,
} from "./ai";
import dayjs from "../dayjs.ts";

/**
 * Offline AI backend (VITE_MOCK=1). Everything here is deterministic so the
 * whole AI surface is demoable without a provider key:
 *  - status can be flipped enabled/disabled (persisted, so the "not enabled"
 *    guidance in the assistant panel / issue card can be exercised in dev);
 *  - assistant answers derive a stable query + explanation + inline result from
 *    the question (magic token "__error" exercises the execution-error branch);
 *  - issue analyses are generated once per id and stored for the tab lifetime.
 * Generated prose is emitted in the active UI language (it is model *output*,
 * i.e. data — not chrome — but a bilingual demo reads better either way).
 */

const MOCK_AI_SETTINGS_KEY = "lo-mock-ai-settings";

/** Default mock configuration: disabled (privacy posture), provider openai. */
function defaultMockSettings(): AiSettings {
  return {
    enabled: false,
    provider: "",
    base_url: "",
    model: "",
    max_tokens: 1024,
    has_api_key: false,
    source: "database",
    updated_by: "admin",
  };
}

function readMockSettings(): AiSettings {
  try {
    const raw = localStorage.getItem(MOCK_AI_SETTINGS_KEY);
    if (!raw) return defaultMockSettings();
    const parsed = JSON.parse(raw) as AiSettings;
    return { ...defaultMockSettings(), ...parsed };
  } catch {
    return defaultMockSettings();
  }
}

function writeMockSettings(settings: AiSettings): void {
  try {
    localStorage.setItem(MOCK_AI_SETTINGS_KEY, JSON.stringify(settings));
  } catch {
    /* ignore */
  }
}

/** Whether the mock AI backend reports itself enabled (default: disabled). */
export function mockAiIsEnabled(): boolean {
  return readMockSettings().enabled;
}

/** Dev-only toggle (surfaced on the AI settings page under VITE_MOCK). */
export function setMockAiEnabled(on: boolean): void {
  const settings = readMockSettings();
  settings.enabled = on;
  writeMockSettings(settings);
}

const delay = <T,>(value: T, ms = 500): Promise<T> =>
  new Promise((resolve) => setTimeout(() => resolve(value), ms));

function ensureEnabled(): void {
  if (!mockAiIsEnabled()) {
    throw new ApiError("AI features are disabled", 409);
  }
}

export async function mockAiStatus(): Promise<AiStatus> {
  const settings = readMockSettings();
  return delay(
    settings.enabled
      ? { enabled: true, provider: settings.provider || "openai", model: settings.model || "gpt-4o-mini" }
      : { enabled: false, provider: "", model: "" },
    120,
  );
}

export async function mockAiSettingsGet(): Promise<AiSettings> {
  return delay(readMockSettings(), 120);
}

export async function mockAiSettingsPut(req: AiSettingsUpdate): Promise<AiSettings> {
  const current = readMockSettings();
  const next: AiSettings = { ...current };
  if (req.enabled !== undefined) next.enabled = req.enabled;
  if (req.provider !== undefined) next.provider = req.provider;
  if (req.base_url !== undefined) next.base_url = req.base_url;
  if (req.model !== undefined) next.model = req.model;
  if (req.max_tokens !== undefined) next.max_tokens = req.max_tokens;
  if (req.clear_api_key) {
    next.has_api_key = false;
  } else if (req.api_key !== undefined && req.api_key.trim() !== "") {
    next.has_api_key = true;
  }
  if (next.enabled && (!next.provider || !next.model)) {
    throw new ApiError("model is required when AI is enabled", 400);
  }
  if (next.enabled && !next.has_api_key) {
    throw new ApiError("API key is required when AI is enabled", 400);
  }
  writeMockSettings(next);
  return delay(next, 250);
}

export async function mockAiTest(): Promise<AiTestResult> {
  ensureEnabled();
  return delay({ latency_ms: 342, echo: "pong" }, 600);
}

// ---- Assistant ------------------------------------------------------------

function hash(s: string): number {
  let h = 2166136261;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return Math.abs(h);
}

const LOG_HINTS = ["log", "日志", "error", "错误", "exception", "异常", "trace", "panic", "stack"];

function resolveSignal(question: string, requested: AiSignal): "metrics" | "logs" {
  if (requested === "metrics" || requested === "logs") return requested;
  const q = question.toLowerCase();
  return LOG_HINTS.some((h) => q.includes(h)) ? "logs" : "metrics";
}

const METRIC_QUERIES = [
  "rate(http_requests_total[5m])",
  "histogram_quantile(0.95, sum(rate(http_request_duration_seconds_bucket[5m])) by (le))",
  "sum(rate(http_requests_total{status=~\"5..\"}[5m])) / sum(rate(http_requests_total[5m]))",
  "avg(rate(process_cpu_seconds_total[5m])) by (instance)",
];

const LOG_QUERIES = [
  '_stream:{app="api-gateway"} error',
  '_stream:{app="checkout"} level:error',
  '_stream:{app="worker"} panic OR exception',
];

/** Language-aware explanation for a generated query (mock/demo prose). */
function explainFor(kind: "metrics" | "logs", query: string): string {
  const zh = i18n.language?.startsWith("zh") ?? true;
  if (kind === "metrics") {
    return zh
      ? `该表达式基于你的问题生成，用于统计对应指标随时间的变化趋势。表达式为 \`${query}\`，在所选时间范围内按步长聚合后绘制为时序曲线。`
      : `Generated from your question to quantify the corresponding metric over time. The expression \`${query}\` is aggregated by step across the selected range and drawn as a line chart.`;
  }
  return zh
    ? `该 LogsQL 表达式用于检索与你问题相关的日志。它筛选目标数据流并匹配关键字，返回最近若干条命中日志。`
    : `This LogsQL expression retrieves logs relevant to your question. It scopes the target stream and matches keywords, returning the most recent hits.`;
}

function resolveMockRange(timeRange?: string | AssistantTimeRange): {
  start: number;
  end: number;
  step: number;
} {
  if (!timeRange || typeof timeRange === "string") {
    return resolveRange(rangeById(timeRange ?? "1h"));
  }
  const parseSec = (value: string | number): number => {
    if (typeof value === "number") return value;
    const numeric = Number(value);
    if (Number.isFinite(numeric)) return numeric;
    return Math.floor(dayjs(value).valueOf() / 1_000);
  };
  const start = parseSec(timeRange.start);
  const end = parseSec(timeRange.end);
  if (Number.isFinite(start) && Number.isFinite(end) && end > start) {
    return { start, end, step: Math.max(1, Math.floor((end - start) / 120)) };
  }
  return resolveRange(rangeById("1h"));
}

async function buildMetricsResult(
  query: string,
  timeRange?: string | AssistantTimeRange,
): Promise<AssistantResult> {
  const { start, end, step } = resolveMockRange(timeRange);
  const resp = mockQueryRange({ query, start, end, step });
  const series: TimeSeries[] =
    resp.status === "success"
      ? resp.data.result.map((r) => ({
          name: seriesLabel(r.metric),
          labels: r.metric,
          points: r.values.map(([t, v]) => {
            const n = Number(v);
            return [t * 1000, Number.isFinite(n) ? n : null] as [number, number | null];
          }),
        }))
      : [];
  return { kind: "metrics", series };
}

async function buildLogsResult(
  query: string,
  timeRange?: string | AssistantTimeRange,
): Promise<AssistantResult> {
  const { start, end } = resolveMockRange(timeRange);
  const rows = await mockQueryLogs({ query, start, end, limit: 20 });
  return { kind: "logs", rows };
}

export async function mockAssistantQuery(
  req: AssistantQueryRequest,
): Promise<AssistantAnswer> {
  ensureEnabled();
  const question = req.question.trim();
  const kind = resolveSignal(question, req.signal ?? "auto");
  const seed = hash(question);

  // Magic token: exercise the execution-error branch (query shown, no result).
  if (question.includes("__error")) {
    const query = kind === "metrics" ? METRIC_QUERIES[0] : LOG_QUERIES[0];
    return delay({
      signal: kind,
      query,
      explanation: explainFor(kind, query),
      executionError:
        i18n.language?.startsWith("zh") ?? true
          ? "查询执行失败：unknown function or metric（示例错误态）"
          : "Query execution failed: unknown function or metric (demo error state)",
    });
  }

  const query =
    kind === "metrics"
      ? METRIC_QUERIES[seed % METRIC_QUERIES.length]
      : LOG_QUERIES[seed % LOG_QUERIES.length];

  const result =
    kind === "metrics"
      ? await buildMetricsResult(query, req.time_range)
      : await buildLogsResult(query, req.time_range);

  return delay({
    signal: kind,
    query,
    explanation: explainFor(kind, query),
    result,
  });
}

export async function mockAssistantExplain(req: {
  signal: "metrics" | "logs";
  query: string;
}): Promise<{ explanation: string }> {
  ensureEnabled();
  return delay({ explanation: explainFor(req.signal, req.query) });
}

// ---- Issue AI analysis ----------------------------------------------------

const store = new Map<string, IssueAiAnalysis>();

/** Half of issue ids are flagged so the "stale -> refresh" flow is demoable. */
function staleEligible(id: string): boolean {
  return hash(id) % 2 === 0;
}

interface AnalysisText {
  summary: string;
  causes: ProbableCause[];
  actions: string[];
}

function buildAnalysis(id: string): AnalysisText {
  const zh = i18n.language?.startsWith("zh") ?? true;
  const seed = hash(id);
  const confidences: ProbableCause["confidence"][] = ["high", "medium", "low"];
  const rot = seed % 3;

  if (zh) {
    return {
      summary:
        "该问题表现为在渲染订单列表时读取了未定义对象的属性，集中出现在最近一次前端发布之后，且主要影响使用旧版缓存资源的用户。错误在高并发结账路径上被放大。",
      causes: [
        {
          cause: "后端返回的订单项字段结构变更，前端未做空值兜底",
          confidence: confidences[rot],
          evidence: "堆栈顶帧位于 CheckoutForm.tsx 的 renderRow，且报错为读取 undefined 的 'map'",
        },
        {
          cause: "旧版本前端资源被 CDN 缓存，与新接口契约不兼容",
          confidence: confidences[(rot + 1) % 3],
          evidence: "受影响会话集中在发布后 10 分钟内，且多来自命中旧 chunk 的用户",
        },
        {
          cause: "并发结账导致的竞态，部分请求在数据就绪前触发渲染",
          confidence: confidences[(rot + 2) % 3],
          evidence: "错误率与请求速率高峰强相关，低峰期几乎不出现",
        },
      ],
      actions: [
        "在 renderRow 中对订单项数组增加空值与默认值兜底",
        "为受影响接口增加契约测试，校验字段结构变更",
        "强制刷新 CDN 缓存并为静态资源加上内容哈希",
        "在结账路径加入加载态守卫，避免数据未就绪即渲染",
      ],
    };
  }
  return {
    summary:
      "This issue is a read of a property on an undefined object while rendering the order list. It clustered right after the latest frontend release and mainly affects users on stale cached assets. The error is amplified on the high-throughput checkout path.",
    causes: [
      {
        cause: "Backend order-item field shape changed; the frontend lacks a null guard",
        confidence: confidences[rot],
        evidence: "Top frame is renderRow in CheckoutForm.tsx and the error reads 'map' of undefined",
      },
      {
        cause: "Older frontend assets cached at the CDN are incompatible with the new API contract",
        confidence: confidences[(rot + 1) % 3],
        evidence: "Affected sessions cluster within 10 min of the release, mostly on a stale chunk",
      },
      {
        cause: "Race under concurrent checkout: some requests render before data is ready",
        confidence: confidences[(rot + 2) % 3],
        evidence: "Error rate correlates strongly with request-rate peaks and is near zero off-peak",
      },
    ],
    actions: [
      "Add null/default guards for the order-item array in renderRow",
      "Add a contract test for the affected endpoint to catch field-shape changes",
      "Purge the CDN cache and add content hashes to static assets",
      "Add a loading guard on the checkout path so rendering waits for data",
    ],
  };
}

function generate(id: string, refresh: boolean): IssueAiAnalysis {
  const text = buildAnalysis(id);
  const analysis: IssueAiAnalysis = {
    summary: text.summary,
    probable_causes: text.causes,
    suggested_actions: text.actions,
    cached: false,
    // A freshly generated analysis is stale only when new events already arrived
    // (mock: deterministic per id); a refresh always clears it.
    stale: refresh ? false : staleEligible(id),
    ai_analysis_at: dayjs().valueOf(),
  };
  store.set(id, analysis);
  return analysis;
}

export async function mockGetIssueAnalysis(id: string): Promise<IssueAiAnalysis | null> {
  const existing = store.get(id);
  if (!existing) return delay(null, 200);
  return delay({ ...existing, cached: true }, 200);
}

export async function mockGenerateIssueAnalysis(
  id: string,
  refresh: boolean,
): Promise<IssueAiAnalysis> {
  ensureEnabled();
  return delay(generate(id, refresh), 900);
}
