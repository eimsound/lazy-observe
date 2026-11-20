/**
 * Strict wire-contract normalizers for Alerting & Channels endpoints
 * (`internal/gateway/alert_api.go`).
 *
 * Kept free of Vite/React imports so contract tests run directly under Vitest.
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

const VALID_CHANNEL_TYPES = new Set<string>(CHANNEL_TYPES);
const VALID_SEVERITIES = new Set<string>(SEVERITIES);

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
  /**
   * Present with redacted secret values (`[REDACTED]`) for admins; empty `{}`
   * when normalized from the minimal member projection (`GET /api/channels`
   * omits `config`, `created_at`, and `updated_at` for non-admin members).
   */
  config: Record<string, string>;
  enabled: boolean;
  created_at: string;
  updated_at: string;
}

export type ChannelInput = Pick<NotificationChannel, "type" | "name" | "config" | "enabled">;

export interface ChannelTestResult {
  ok: boolean;
  message: string;
}

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
  /** May legitimately be `""` when no comment was provided. */
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

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function assertRecord(value: unknown, label: string): Record<string, unknown> {
  if (!isRecord(value)) {
    throw new TypeError(`${label} must be an object`);
  }
  return value;
}

function normalizeId(value: unknown, label: string): string {
  if (typeof value === "string" && value.trim() !== "") return value;
  if (typeof value === "number" && Number.isSafeInteger(value) && value > 0) {
    return String(value);
  }
  throw new TypeError(`${label} must be a non-empty string or positive integer`);
}

function assertString(value: unknown, label: string, allowEmpty = false): string {
  if (typeof value !== "string" || (!allowEmpty && value.trim() === "")) {
    throw new TypeError(`${label} must be a${allowEmpty ? "" : " non-empty"} string`);
  }
  return value;
}

function optionalString(value: unknown, label: string): string {
  if (value === undefined || value === null) return "";
  if (typeof value !== "string") {
    throw new TypeError(`${label} must be a string`);
  }
  return value;
}

function assertBoolean(value: unknown, label: string): boolean {
  if (typeof value !== "boolean") {
    throw new TypeError(`${label} must be a boolean`);
  }
  return value;
}

function normalizeStringRecord(value: unknown, label: string): Record<string, string> {
  if (value === undefined || value === null) return {};
  if (!isRecord(value)) {
    throw new TypeError(`${label} must be an object`);
  }
  return Object.fromEntries(
    Object.entries(value).map(([key, item]) => [
      key,
      typeof item === "string" ? item : Array.isArray(item) ? item.join(",") : String(item ?? ""),
    ]),
  );
}

function normalizeChannelType(value: unknown, label: string): ChannelType {
  const raw = assertString(value, label).trim().toLowerCase();
  const mapped = raw === "email" ? "smtp" : raw;
  if (!VALID_CHANNEL_TYPES.has(mapped)) {
    throw new TypeError(`${label} must be one of ${CHANNEL_TYPES.join(", ")}`);
  }
  return mapped as ChannelType;
}

/**
 * Validates and normalizes `GET /api/channels`, `POST /api/channels`, and
 * `PATCH /api/channels/:id` responses (`internal/gateway/alert_api.go`).
 *
 * Accepts both:
 * - Admin full redacted projection (`notificationChannelResponse`)
 * - Member minimal read-only projection (`notificationChannelSummaryResponse`,
 *   which intentionally omits `config`, `created_at`, and `updated_at`).
 */
export function normalizeChannel(value: unknown): NotificationChannel {
  const raw = assertRecord(value, "notification channel");
  return {
    id: normalizeId(raw.id, "channel.id"),
    type: normalizeChannelType(raw.type, "channel.type"),
    name: assertString(raw.name, "channel.name"),
    config: normalizeStringRecord(raw.config, "channel.config"),
    enabled: assertBoolean(raw.enabled, "channel.enabled"),
    created_at: optionalString(raw.created_at, "channel.created_at"),
    updated_at: optionalString(raw.updated_at, "channel.updated_at"),
  };
}

export function normalizeChannelList(value: unknown): NotificationChannel[] {
  if (value === null || value === undefined) return [];
  if (!Array.isArray(value)) {
    throw new TypeError("channels response must be an array");
  }
  return value.map(normalizeChannel);
}

/** Validates `POST /api/channels/:id/test` HTTP 200 JSON (`channelTestResponse`). */
export function normalizeChannelTestResult(value: unknown): ChannelTestResult {
  const raw = assertRecord(value, "channel test response");
  return {
    ok: assertBoolean(raw.ok, "channel test.ok"),
    message: assertString(raw.message, "channel test.message", true),
  };
}

/** Validates `GET`/`POST`/`PATCH /api/alert-rules` (`alertRuleResponse`). */
export function normalizeAlertRule(value: unknown): AlertRule {
  const raw = assertRecord(value, "alert rule");
  const severityRaw = assertString(raw.severity, "alert rule.severity").trim().toLowerCase();
  if (!VALID_SEVERITIES.has(severityRaw)) {
    throw new TypeError(`alert rule.severity must be one of ${SEVERITIES.join(", ")}`);
  }
  const channelIDs = raw.channel_ids === undefined || raw.channel_ids === null
    ? []
    : Array.isArray(raw.channel_ids)
      ? raw.channel_ids.map((item, idx) => normalizeId(item, `alert rule.channel_ids[${idx}]`))
      : (() => {
        throw new TypeError("alert rule.channel_ids must be an array");
      })();

  return {
    id: normalizeId(raw.id, "alert rule.id"),
    name: assertString(raw.name, "alert rule.name"),
    signal: raw.signal === "logs" ? "logs" : "metrics",
    evaluation_engine: raw.evaluation_engine === "paused" ? "paused" : "native",
    expr: assertString(raw.expr, "alert rule.expr"),
    for: optionalString(raw.for, "alert rule.for"),
    severity: severityRaw as Severity,
    labels: normalizeStringRecord(raw.labels, "alert rule.labels"),
    annotations: normalizeStringRecord(raw.annotations, "alert rule.annotations"),
    enabled: assertBoolean(raw.enabled, "alert rule.enabled"),
    channel_ids: channelIDs,
    created_at: optionalString(raw.created_at, "alert rule.created_at"),
    updated_at: optionalString(raw.updated_at, "alert rule.updated_at"),
  };
}

export function normalizeAlertRuleList(value: unknown): AlertRule[] {
  if (value === null || value === undefined) return [];
  if (!Array.isArray(value)) {
    throw new TypeError("alert rules response must be an array");
  }
  return value.map(normalizeAlertRule);
}

/**
 * Validates `GET`/`POST`/`PATCH /api/silences` (`silenceResponse`).
 * Explicitly permits empty `comment: ""` (and omitted `comment` -> `""`),
 * matching `alert.SilenceInput` where comment is optional.
 */
export function normalizeSilence(value: unknown): Silence {
  const raw = assertRecord(value, "silence");
  if (!Array.isArray(raw.matchers)) {
    throw new TypeError("silence.matchers must be an array");
  }
  const matchers: Matcher[] = raw.matchers.map((entry, idx) => {
    const matcher = assertRecord(entry, `silence.matchers[${idx}]`);
    const op = matcher.op;
    if (op !== "=" && op !== "=~") {
      throw new TypeError(`silence.matchers[${idx}].op must be "=" or "=~"`);
    }
    return {
      name: assertString(matcher.name, `silence.matchers[${idx}].name`),
      op,
      value: assertString(matcher.value, `silence.matchers[${idx}].value`, true),
    };
  });

  const creator = raw.creator !== undefined
    ? optionalString(raw.creator, "silence.creator")
    : optionalString(raw.created_by, "silence.created_by");

  return {
    id: normalizeId(raw.id, "silence.id"),
    matchers,
    starts_at: assertString(raw.starts_at, "silence.starts_at"),
    ends_at: assertString(raw.ends_at, "silence.ends_at"),
    comment: optionalString(raw.comment, "silence.comment"),
    creator,
    created_at: optionalString(raw.created_at, "silence.created_at"),
  };
}

export function normalizeSilenceList(value: unknown): Silence[] {
  if (value === null || value === undefined) return [];
  if (!Array.isArray(value)) {
    throw new TypeError("silences response must be an array");
  }
  return value.map(normalizeSilence);
}

/** Validates `GET /api/alerts` (`alertEventResponse`). */
export function normalizeAlertEvent(value: unknown): AlertEvent {
  const raw = assertRecord(value, "alert event");
  const status = raw.status;
  if (status !== "firing" && status !== "resolved") {
    throw new TypeError('alert event.status must be "firing" or "resolved"');
  }
  const labels = normalizeStringRecord(raw.labels, "alert event.labels");
  const annotations = normalizeStringRecord(raw.annotations, "alert event.annotations");
  const ruleField = raw.rule;
  const rule = typeof ruleField === "string"
    ? ruleField
    : isRecord(ruleField) && typeof ruleField.name === "string"
      ? ruleField.name
      : (labels.alertname ?? "");
  const fingerprint = assertString(raw.fingerprint, "alert event.fingerprint");
  const endsAt = raw.ends_at === undefined || raw.ends_at === null
    ? undefined
    : assertString(raw.ends_at, "alert event.ends_at", true);

  return {
    id: raw.id !== undefined && raw.id !== null ? normalizeId(raw.id, "alert event.id") : fingerprint,
    fingerprint,
    rule,
    status,
    labels,
    annotations,
    starts_at: assertString(raw.starts_at, "alert event.starts_at"),
    ...(endsAt ? { ends_at: endsAt } : {}),
    inhibited: raw.inhibited === true,
    updated_at: optionalString(raw.updated_at, "alert event.updated_at"),
  };
}

export function normalizeAlertEventList(value: unknown): AlertEvent[] {
  if (value === null || value === undefined) return [];
  if (!Array.isArray(value)) {
    throw new TypeError("alert events response must be an array");
  }
  return value.map(normalizeAlertEvent);
}
