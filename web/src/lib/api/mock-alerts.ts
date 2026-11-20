import type {
  AlertRule,
  AlertRuleInput,
  AlertEvent,
  NotificationChannel,
  ChannelInput,
  Silence,
  SilenceInput,
} from "./alerts";
import { hexId } from "./mock-random";
import dayjs from "../dayjs.ts";

/**
 * In-memory alerting store for VITE_MOCK mode. Rules / channels / silences are
 * mutable for the tab lifetime; the event list is derived from the current rule
 * set on each read so firing/resolved counts stay consistent with the rules.
 */

let counter = 1;
const nextId = (prefix: string) => `${prefix}_${hexId(`${prefix}${counter++}`, 10)}`;
const ago = (mins: number) => dayjs().subtract(mins, "minute").toISOString();
const ahead = (mins: number) => dayjs().add(mins, "minute").toISOString();

export const mockChannels: NotificationChannel[] = [
  {
    id: "chn_ops_webhook",
    type: "webhook",
    name: "Ops Webhook",
    config: { url: "https://hooks.example.com/lo/ops", secret: "[REDACTED]" },
    enabled: true,
    created_at: ago(60 * 24 * 30),
    updated_at: ago(60 * 24 * 2),
  },
  {
    id: "chn_feishu_sre",
    type: "feishu",
    name: "SRE 飞书群",
    config: { url: "https://open.feishu.cn/open-apis/bot/v2/hook/xxxx" },
    enabled: true,
    created_at: ago(60 * 24 * 20),
    updated_at: ago(60 * 24),
  },
  {
    id: "chn_email_oncall",
    type: "smtp",
    name: "On-call 邮件",
    config: {
      to: "oncall@example.com",
      from: "alerts@example.com",
      host: "smtp.example.com",
      port: "587",
    },
    enabled: false,
    created_at: ago(60 * 24 * 10),
    updated_at: ago(60 * 12),
  },
];

export const mockRules: AlertRule[] = [
  {
    id: "rule_high_error_rate",
    name: "HighErrorRate",
    signal: "metrics",
    evaluation_engine: "native",
    expr: 'sum(rate(http_requests_total{status=~"5.."}[5m])) / sum(rate(http_requests_total[5m])) > 0.05',
    for: "5m",
    severity: "critical",
    labels: { team: "platform" },
    annotations: {
      summary: "5xx 错误率超过 5%",
      description: "服务 {{ $labels.service }} 的错误率持续高于阈值。",
    },
    enabled: true,
    channel_ids: ["chn_ops_webhook", "chn_feishu_sre"],
    created_at: ago(60 * 24 * 30),
    updated_at: ago(10),
  },
  {
    id: "rule_high_latency",
    name: "HighRequestLatencyP95",
    signal: "metrics",
    evaluation_engine: "native",
    expr: "histogram_quantile(0.95, sum(rate(http_request_duration_seconds_bucket[5m])) by (le)) > 0.5",
    for: "10m",
    severity: "warning",
    labels: { team: "platform" },
    annotations: { summary: "P95 时延超过 500ms" },
    enabled: true,
    channel_ids: ["chn_feishu_sre"],
    created_at: ago(60 * 24 * 20),
    updated_at: ago(20),
  },
  {
    id: "rule_disk_space",
    name: "DiskSpaceLow",
    signal: "metrics",
    evaluation_engine: "native",
    expr: 'node_filesystem_avail_bytes / node_filesystem_size_bytes < 0.1',
    for: "15m",
    severity: "warning",
    labels: { team: "infra" },
    annotations: { summary: "磁盘可用空间低于 10%" },
    enabled: false,
    channel_ids: [],
    created_at: ago(60 * 24 * 15),
    updated_at: ago(30),
  },
  {
    id: "rule_target_down",
    name: "TargetDown",
    signal: "metrics",
    evaluation_engine: "native",
    expr: "up == 0",
    for: "2m",
    severity: "critical",
    labels: { team: "infra" },
    annotations: { summary: "监控目标不可达" },
    enabled: true,
    channel_ids: ["chn_ops_webhook"],
    created_at: ago(60 * 24 * 10),
    updated_at: ago(40),
  },
];

export const mockSilences: Silence[] = [
  {
    id: "sil_deploy",
    matchers: [{ name: "service", op: "=", value: "checkout" }],
    starts_at: ago(30),
    ends_at: ahead(90),
    comment: "checkout 发布窗口，静默相关告警",
    creator: "admin",
    created_at: ago(30),
  },
  {
    id: "sil_maintenance",
    matchers: [
      { name: "team", op: "=", value: "infra" },
      { name: "instance", op: "=~", value: "db-.*" },
    ],
    starts_at: ago(60 * 24),
    ends_at: ago(60 * 12),
    comment: "数据库维护（已过期）",
    creator: "alice",
    created_at: ago(60 * 24),
  },
];

// ── rule ops ─────────────────────────────────────────────────────────────────

export function createMockRule(input: AlertRuleInput): AlertRule {
  const now = dayjs().toISOString();
  const rule: AlertRule = { id: nextId("rule"), ...input, evaluation_engine: input.evaluation_engine ?? "native", created_at: now, updated_at: now };
  mockRules.unshift(rule);
  return rule;
}

export function updateMockRule(id: string, input: AlertRuleInput): AlertRule {
  const idx = mockRules.findIndex((r) => r.id === id);
  if (idx < 0) throw new Error("rule not found");
  mockRules[idx] = {
    id,
    ...input,
    evaluation_engine: input.evaluation_engine ?? mockRules[idx].evaluation_engine,
    created_at: mockRules[idx].created_at,
    updated_at: dayjs().toISOString(),
  };
  return mockRules[idx];
}

export function deleteMockRule(id: string): void {
  const idx = mockRules.findIndex((r) => r.id === id);
  if (idx >= 0) mockRules.splice(idx, 1);
}

// ── channel ops ──────────────────────────────────────────────────────────────

export function createMockChannel(input: ChannelInput): NotificationChannel {
  const now = dayjs().toISOString();
  const ch: NotificationChannel = { id: nextId("chn"), ...input, created_at: now, updated_at: now };
  mockChannels.push(ch);
  return ch;
}

export function updateMockChannel(id: string, input: ChannelInput): NotificationChannel {
  const idx = mockChannels.findIndex((c) => c.id === id);
  if (idx < 0) throw new Error("channel not found");
  mockChannels[idx] = {
    id,
    ...input,
    created_at: mockChannels[idx].created_at,
    updated_at: dayjs().toISOString(),
  };
  return mockChannels[idx];
}

export function deleteMockChannel(id: string): void {
  const idx = mockChannels.findIndex((c) => c.id === id);
  if (idx >= 0) mockChannels.splice(idx, 1);
  // Detach the deleted channel from any rule referencing it.
  for (const r of mockRules) {
    r.channel_ids = r.channel_ids.filter((cid) => cid !== id);
  }
}

// ── silence ops ──────────────────────────────────────────────────────────────

export function createMockSilence(input: SilenceInput): Silence {
  const s: Silence = {
    id: nextId("sil"),
    ...input,
    creator: "mock-user",
    created_at: dayjs().toISOString(),
  };
  mockSilences.unshift(s);
  return s;
}

export function updateMockSilence(id: string, input: SilenceInput): Silence {
  const idx = mockSilences.findIndex((s) => s.id === id);
  if (idx < 0) throw new Error("silence not found");
  mockSilences[idx] = {
    id,
    ...input,
    creator: mockSilences[idx].creator,
    created_at: mockSilences[idx].created_at,
  };
  return mockSilences[idx];
}

export function deleteMockSilence(id: string): void {
  const idx = mockSilences.findIndex((s) => s.id === id);
  if (idx >= 0) mockSilences.splice(idx, 1);
}

// ── events (derived) ─────────────────────────────────────────────────────────

const SERVICES = ["checkout", "orders", "auth", "payments", "gateway"];
const INSTANCES = ["10.0.0.11:9100", "10.0.0.12:9100", "10.0.0.13:9100"];

/**
 * Derive an event list from the enabled rules. Enabled rules produce firing
 * events; a couple of resolved events give the resolved filter something to
 * show. Stable per rule so the list doesn't churn between reads.
 */
export function mockEvents(): AlertEvent[] {
  const events: AlertEvent[] = [];
  const enabled = mockRules.filter((r) => r.enabled);

  enabled.forEach((rule, i) => {
    const service = SERVICES[i % SERVICES.length];
    const instance = INSTANCES[i % INSTANCES.length];
    events.push({
      id: `evt_${hexId(`id|${rule.id}|firing`, 12)}`,
      fingerprint: hexId(`fp|${rule.id}|firing`, 16),
      rule: rule.name,
      status: "firing",
      labels: {
        alertname: rule.name,
        severity: rule.severity,
        service,
        instance,
        ...rule.labels,
      },
      annotations: rule.annotations,
      starts_at: ago(6 + i * 11),
      inhibited: false,
      updated_at: ago(i),
    });
  });

  // Two resolved events (independent of current rules) for the resolved tab.
  events.push({
    id: `evt_${hexId("id|resolved|1", 12)}`,
    fingerprint: hexId("fp|resolved|1", 16),
    rule: "HighErrorRate",
    status: "resolved",
    labels: { alertname: "HighErrorRate", severity: "critical", service: "orders" },
    annotations: { summary: "5xx 错误率超过 5%" },
    starts_at: ago(240),
    ends_at: ago(180),
    inhibited: false,
    updated_at: ago(180),
  });
  events.push({
    id: `evt_${hexId("id|resolved|2", 12)}`,
    fingerprint: hexId("fp|resolved|2", 16),
    rule: "HighRequestLatencyP95",
    status: "resolved",
    labels: { alertname: "HighRequestLatencyP95", severity: "warning", service: "auth" },
    annotations: { summary: "P95 时延超过 500ms" },
    starts_at: ago(400),
    ends_at: ago(360),
    inhibited: false,
    updated_at: ago(360),
  });

  return events;
}
