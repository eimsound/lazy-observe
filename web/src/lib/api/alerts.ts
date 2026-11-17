import { apiGet, apiSend, IS_MOCK, API_ROOT } from "./client";

// Mock data is only for VITE_MOCK builds; load it lazily so it never ships with real chunks.
const loadMockAlerts = () => import("./mock-alerts");

/**
 * Alerting API (A1). Fixed contract, endpoints under /api:
 *   /api/alert-rules  CRUD
 *   /api/channels     CRUD + POST /api/channels/:id/test
 *   /api/silences     CRUD
 *   GET /api/alerts?status=firing|resolved  -> event list
 * In VITE_MOCK mode an in-memory store backs every operation so the whole
 * alerting surface is interactive offline.
 */

export type Severity = "critical" | "warning" | "info";
export const SEVERITIES: Severity[] = ["critical", "warning", "info"];

export type AlertSignal = "metrics" | "logs";
export const ALERT_SIGNALS: AlertSignal[] = ["metrics", "logs"];

export type ChannelType = "webhook" | "feishu" | "dingtalk" | "wecom" | "smtp" | "slack";
export const CHANNEL_TYPES: ChannelType[] = [
  "webhook",
  "feishu",
  "dingtalk",
  "wecom",
  "smtp",
  "slack",
];

export interface AlertRule {
  id: string;
  name: string;
  signal: AlertSignal;
  evaluation_engine: "paused" | "native";
  /** MetricsQL/PromQL or LogsQL expression the alert fires on. */
  expr: string;
  /** Duration the condition must hold before firing, e.g. "5m". */
  for: string;
  severity: Severity;
  labels: Record<string, string>;
  annotations: Record<string, string>;
  enabled: boolean;
  channel_ids: string[];
  created_at: string;
  updated_at: string;
}

/** Payload for create/update (server assigns identity and timestamps). */
export type AlertRuleInput = Pick<
  AlertRule,
  "name" | "signal" | "expr" | "for" | "severity" | "labels" | "annotations" | "enabled" | "channel_ids"
> & { evaluation_engine?: "native" };

export interface NotificationChannel {
  id: string;
  type: ChannelType;
  name: string;
  config: Record<string, string>;
  enabled: boolean;
  created_at: string;
  updated_at: string;
}

export type ChannelInput = Pick<NotificationChannel, "type" | "name" | "config" | "enabled">;

export interface Matcher {
  name: string;
  op: "=" | "=~";
  value: string;
}

export interface Silence {
  id: string;
  matchers: Matcher[];
  starts_at: string;
  ends_at: string;
  comment: string;
  creator: string;
  created_at: string;
}

export type SilenceInput = Pick<Silence, "matchers" | "starts_at" | "ends_at" | "comment">;

export type AlertStatus = "firing" | "resolved";

export interface AlertEvent {
  id: string;
  fingerprint: string;
  rule: string;
  status: AlertStatus;
  labels: Record<string, string>;
  annotations: Record<string, string>;
  starts_at: string;
  ends_at?: string;
  inhibited: boolean;
  updated_at: string;
}

function stringRecord(value: unknown): Record<string, string> {
  if (!value || typeof value !== "object" || Array.isArray(value)) return {};
  return Object.fromEntries(
    Object.entries(value).map(([key, item]) => [
      key,
      typeof item === "string" ? item : Array.isArray(item) ? item.join(",") : String(item ?? ""),
    ]),
  );
}

function normalizeRule(value: AlertRule): AlertRule {
  return {
    ...value,
    id: String(value.id),
    signal: value.signal === "logs" ? "logs" : "metrics",
    evaluation_engine: value.evaluation_engine === "paused" ? "paused" : "native",
    labels: value.labels ?? {},
    annotations: value.annotations ?? {},
    channel_ids: Array.isArray(value.channel_ids) ? value.channel_ids.map(String) : [],
    created_at: value.created_at ?? "",
    updated_at: value.updated_at ?? "",
  };
}

function normalizeChannel(value: NotificationChannel): NotificationChannel {
  const wireType = value.type as string;
  return {
    ...value,
    id: String(value.id),
    type: (wireType === "email" ? "smtp" : wireType) as ChannelType,
    config: stringRecord(value.config),
    created_at: value.created_at ?? "",
    updated_at: value.updated_at ?? "",
  };
}

function normalizeSilence(value: Silence): Silence {
  const legacy = value as Silence & { created_by?: string };
  return {
    ...value,
    id: String(value.id),
    matchers: Array.isArray(value.matchers) ? value.matchers : [],
    creator: value.creator ?? legacy.created_by ?? "",
    created_at: value.created_at ?? "",
  };
}

function normalizeEvent(value: AlertEvent): AlertEvent {
  const legacyRule = value.rule as unknown;
  return {
    ...value,
    id: String(value.id ?? value.fingerprint),
    rule:
      typeof legacyRule === "string"
        ? legacyRule
        : ((legacyRule as { name?: string } | null)?.name ?? value.labels?.alertname ?? ""),
    labels: value.labels ?? {},
    annotations: value.annotations ?? {},
    inhibited: value.inhibited ?? false,
    updated_at: value.updated_at ?? "",
  };
}

function delay<T>(value: T, ms = 200): Promise<T> {
  return new Promise((resolve) => setTimeout(() => resolve(value), ms));
}

// ── rules ────────────────────────────────────────────────────────────────────

export async function listRules(signal?: AbortSignal): Promise<AlertRule[]> {
  if (IS_MOCK) return delay([...(await loadMockAlerts()).mockRules]);
  const values = await apiGet<AlertRule[]>("/alert-rules", { base: API_ROOT, signal });
  return (values ?? []).map(normalizeRule);
}

export async function createRule(input: AlertRuleInput): Promise<AlertRule> {
  if (IS_MOCK) return delay((await loadMockAlerts()).createMockRule(input), 250);
  return normalizeRule(await apiSend<AlertRule>("POST", "/alert-rules", input, { base: API_ROOT }));
}

export async function updateRule(id: string, input: AlertRuleInput): Promise<AlertRule> {
  if (IS_MOCK) return delay((await loadMockAlerts()).updateMockRule(id, input), 250);
  return normalizeRule(await apiSend<AlertRule>("PATCH", `/alert-rules/${id}`, input, { base: API_ROOT }));
}

export async function deleteRule(id: string): Promise<void> {
  if (IS_MOCK) {
    (await loadMockAlerts()).deleteMockRule(id);
    return delay(undefined);
  }
  await apiSend<void>("DELETE", `/alert-rules/${id}`, undefined, { base: API_ROOT });
}

// ── channels ─────────────────────────────────────────────────────────────────

export async function listChannels(signal?: AbortSignal): Promise<NotificationChannel[]> {
  if (IS_MOCK) return delay([...(await loadMockAlerts()).mockChannels]);
  const values = await apiGet<NotificationChannel[]>("/channels", { base: API_ROOT, signal });
  return (values ?? []).map(normalizeChannel);
}

export async function createChannel(input: ChannelInput): Promise<NotificationChannel> {
  if (IS_MOCK) return delay((await loadMockAlerts()).createMockChannel(input), 250);
  return normalizeChannel(await apiSend<NotificationChannel>("POST", "/channels", input, { base: API_ROOT }));
}

export async function updateChannel(
  id: string,
  input: ChannelInput,
): Promise<NotificationChannel> {
  if (IS_MOCK) return delay((await loadMockAlerts()).updateMockChannel(id, input), 250);
  return normalizeChannel(
    await apiSend<NotificationChannel>("PATCH", `/channels/${id}`, input, { base: API_ROOT }),
  );
}

export async function deleteChannel(id: string): Promise<void> {
  if (IS_MOCK) {
    (await loadMockAlerts()).deleteMockChannel(id);
    return delay(undefined);
  }
  await apiSend<void>("DELETE", `/channels/${id}`, undefined, { base: API_ROOT });
}

export interface ChannelTestResult {
  ok: boolean;
  message: string;
}

export async function testChannel(id: string): Promise<ChannelTestResult> {
  if (IS_MOCK) {
    const ch = (await loadMockAlerts()).mockChannels.find((c) => c.id === id);
    // A channel with no destination configured "fails" the test — exercises the
    // error path in the UI.
    const hasTarget = ch
      ? Object.values(ch.config).some((v) => v.trim().length > 0)
      : false;
    return delay(
      hasTarget
        ? { ok: true, message: "测试消息已发送" }
        : { ok: false, message: "渠道未配置有效的接收地址" },
      700,
    );
  }
  return apiSend<ChannelTestResult>("POST", `/channels/${id}/test`, undefined, {
    base: API_ROOT,
  });
}

// ── silences ─────────────────────────────────────────────────────────────────

export async function listSilences(signal?: AbortSignal): Promise<Silence[]> {
  if (IS_MOCK) return delay([...(await loadMockAlerts()).mockSilences]);
  const values = await apiGet<Silence[]>("/silences", { base: API_ROOT, signal });
  return (values ?? []).map(normalizeSilence);
}

export async function createSilence(input: SilenceInput): Promise<Silence> {
  if (IS_MOCK) return delay((await loadMockAlerts()).createMockSilence(input), 250);
  return normalizeSilence(await apiSend<Silence>("POST", "/silences", input, { base: API_ROOT }));
}

export async function updateSilence(id: string, input: SilenceInput): Promise<Silence> {
  if (IS_MOCK) return delay((await loadMockAlerts()).updateMockSilence(id, input), 250);
  return normalizeSilence(await apiSend<Silence>("PATCH", `/silences/${id}`, input, { base: API_ROOT }));
}

export async function deleteSilence(id: string): Promise<void> {
  if (IS_MOCK) {
    (await loadMockAlerts()).deleteMockSilence(id);
    return delay(undefined);
  }
  await apiSend<void>("DELETE", `/silences/${id}`, undefined, { base: API_ROOT });
}

// ── events ───────────────────────────────────────────────────────────────────

export async function listAlerts(
  status?: AlertStatus,
  signal?: AbortSignal,
): Promise<AlertEvent[]> {
  if (IS_MOCK) {
    const all = (await loadMockAlerts()).mockEvents();
    return delay(status ? all.filter((e) => e.status === status) : all, 250);
  }
  const values = await apiGet<AlertEvent[]>("/alerts", {
    base: API_ROOT,
    params: status ? { status } : undefined,
    signal,
  });
  return (values ?? []).map(normalizeEvent);
}

/** Severity of an event, derived from its `severity` label (defaults to info). */
export function eventSeverity(e: AlertEvent): Severity {
  const s = e.labels.severity;
  return s === "critical" || s === "warning" || s === "info" ? s : "info";
}
