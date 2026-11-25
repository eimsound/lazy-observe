import { apiGet, apiSend, IS_MOCK, API_ROOT } from "./client";
import {
  ALERT_SIGNALS,
  CHANNEL_TYPES,
  DEFAULT_EVALUATION_INTERVAL,
  EVALUATION_INTERVAL_BOUNDS,
  evaluationIntervalProblem,
  SEVERITIES,
  normalizeAlertEventList,
  normalizeAlertRule,
  normalizeAlertRuleList,
  normalizeChannel,
  normalizeChannelList,
  normalizeChannelTestResult,
  normalizeInhibitionRule,
  normalizeInhibitionRuleList,
  normalizeSilence,
  normalizeSilenceList,
  type AlertEvent,
  type AlertRule,
  type AlertRuleInput,
  type AlertSignal,
  type AlertStatus,
  type ChannelInput,
  type ChannelTestResult,
  type ChannelType,
  type InhibitionRule,
  type InhibitionRuleInput,
  type InhibitionRulePatch,
  type Matcher,
  type NotificationChannel,
  type Severity,
  type Silence,
  type SilenceInput,
} from "./alerts-contract";

// Mock data is only for VITE_MOCK builds; load it lazily so it never ships with real chunks.
const loadMockAlerts = () => import("./mock-alerts");

export {
  ALERT_SIGNALS,
  CHANNEL_TYPES,
  DEFAULT_EVALUATION_INTERVAL,
  EVALUATION_INTERVAL_BOUNDS,
  SEVERITIES,
  evaluationIntervalProblem,
};
export type {
  AlertEvent,
  AlertRule,
  AlertRuleInput,
  AlertSignal,
  AlertStatus,
  ChannelInput,
  ChannelTestResult,
  ChannelType,
  InhibitionRule,
  InhibitionRuleInput,
  InhibitionRulePatch,
  Matcher,
  NotificationChannel,
  Severity,
  Silence,
  SilenceInput,
};

/**
 * Alerting API (A1). Fixed contract, endpoints under /api:
 *   /api/alert-rules       CRUD (project-scoped, member+)
 *   /api/channels          GET (member+ summary / admin redacted), write+test (admin)
 *   /api/silences          CRUD (project-scoped, member+)
 *   /api/inhibition-rules  CRUD (project-scoped, member+)
 *   GET /api/alerts?status=firing|resolved  -> event list (project-scoped, viewer+)
 * In VITE_MOCK mode an in-memory store backs every operation so the whole
 * alerting surface is interactive offline.
 */

function delay<T>(value: T, ms = 200): Promise<T> {
  return new Promise((resolve) => setTimeout(() => resolve(value), ms));
}

// ── rules ────────────────────────────────────────────────────────────────────

export async function listRules(signal?: AbortSignal, project?: string): Promise<AlertRule[]> {
  if (IS_MOCK) return delay([...(await loadMockAlerts()).mockRules]);
  const values = await apiGet<unknown>("/alert-rules", { base: API_ROOT, signal, project });
  return normalizeAlertRuleList(values);
}

export async function createRule(input: AlertRuleInput, project?: string): Promise<AlertRule> {
  if (IS_MOCK) return delay((await loadMockAlerts()).createMockRule(input), 250);
  return normalizeAlertRule(
    await apiSend<unknown>("POST", "/alert-rules", input, { base: API_ROOT, project }),
  );
}

export async function updateRule(
  id: string,
  input: AlertRuleInput,
  project?: string,
): Promise<AlertRule> {
  if (IS_MOCK) return delay((await loadMockAlerts()).updateMockRule(id, input), 250);
  return normalizeAlertRule(
    await apiSend<unknown>("PATCH", `/alert-rules/${id}`, input, { base: API_ROOT, project }),
  );
}

export async function deleteRule(id: string, project?: string): Promise<void> {
  if (IS_MOCK) {
    (await loadMockAlerts()).deleteMockRule(id);
    return delay(undefined);
  }
  await apiSend<void>("DELETE", `/alert-rules/${id}`, undefined, { base: API_ROOT, project });
}

// ── channels ─────────────────────────────────────────────────────────────────

export async function listChannels(signal?: AbortSignal): Promise<NotificationChannel[]> {
  if (IS_MOCK) return delay([...(await loadMockAlerts()).mockChannels]);
  const values = await apiGet<unknown>("/channels", { base: API_ROOT, signal });
  return normalizeChannelList(values);
}

export async function createChannel(input: ChannelInput): Promise<NotificationChannel> {
  if (IS_MOCK) return delay((await loadMockAlerts()).createMockChannel(input), 250);
  return normalizeChannel(
    await apiSend<unknown>("POST", "/channels", input, { base: API_ROOT }),
  );
}

export async function updateChannel(
  id: string,
  input: ChannelInput,
): Promise<NotificationChannel> {
  if (IS_MOCK) return delay((await loadMockAlerts()).updateMockChannel(id, input), 250);
  return normalizeChannel(
    await apiSend<unknown>("PATCH", `/channels/${id}`, input, { base: API_ROOT }),
  );
}

export async function deleteChannel(id: string): Promise<void> {
  if (IS_MOCK) {
    (await loadMockAlerts()).deleteMockChannel(id);
    return delay(undefined);
  }
  await apiSend<void>("DELETE", `/channels/${id}`, undefined, { base: API_ROOT });
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
  return normalizeChannelTestResult(
    await apiSend<unknown>("POST", `/channels/${id}/test`, undefined, {
      base: API_ROOT,
    }),
  );
}

// ── silences ─────────────────────────────────────────────────────────────────

export async function listSilences(signal?: AbortSignal, project?: string): Promise<Silence[]> {
  if (IS_MOCK) return delay([...(await loadMockAlerts()).mockSilences]);
  const values = await apiGet<unknown>("/silences", { base: API_ROOT, signal, project });
  return normalizeSilenceList(values);
}

export async function createSilence(input: SilenceInput, project?: string): Promise<Silence> {
  if (IS_MOCK) return delay((await loadMockAlerts()).createMockSilence(input), 250);
  return normalizeSilence(
    await apiSend<unknown>("POST", "/silences", input, { base: API_ROOT, project }),
  );
}

export async function updateSilence(
  id: string,
  input: SilenceInput,
  project?: string,
): Promise<Silence> {
  if (IS_MOCK) return delay((await loadMockAlerts()).updateMockSilence(id, input), 250);
  return normalizeSilence(
    await apiSend<unknown>("PATCH", `/silences/${id}`, input, { base: API_ROOT, project }),
  );
}

export async function deleteSilence(id: string, project?: string): Promise<void> {
  if (IS_MOCK) {
    (await loadMockAlerts()).deleteMockSilence(id);
    return delay(undefined);
  }
  await apiSend<void>("DELETE", `/silences/${id}`, undefined, { base: API_ROOT, project });
}

// ── inhibition rules ─────────────────────────────────────────────────────────

export async function listInhibitionRules(
  signal?: AbortSignal,
  project?: string,
): Promise<InhibitionRule[]> {
  if (IS_MOCK) return delay([...(await loadMockAlerts()).mockInhibitionRules]);
  const values = await apiGet<unknown>("/inhibition-rules", { base: API_ROOT, signal, project });
  return normalizeInhibitionRuleList(values);
}

export async function createInhibitionRule(
  input: InhibitionRuleInput,
  project?: string,
): Promise<InhibitionRule> {
  if (IS_MOCK) return delay((await loadMockAlerts()).createMockInhibitionRule(input), 250);
  return normalizeInhibitionRule(
    await apiSend<unknown>("POST", "/inhibition-rules", input, { base: API_ROOT, project }),
  );
}

export async function updateInhibitionRule(
  id: string,
  input: InhibitionRulePatch,
  project?: string,
): Promise<InhibitionRule> {
  if (IS_MOCK) return delay((await loadMockAlerts()).updateMockInhibitionRule(id, input), 250);
  return normalizeInhibitionRule(
    await apiSend<unknown>("PATCH", `/inhibition-rules/${id}`, input, { base: API_ROOT, project }),
  );
}

export async function deleteInhibitionRule(id: string, project?: string): Promise<void> {
  if (IS_MOCK) {
    (await loadMockAlerts()).deleteMockInhibitionRule(id);
    return delay(undefined);
  }
  await apiSend<void>("DELETE", `/inhibition-rules/${id}`, undefined, { base: API_ROOT, project });
}

// ── events ───────────────────────────────────────────────────────────────────

export async function listAlerts(
  status?: AlertStatus,
  signal?: AbortSignal,
  project?: string,
): Promise<AlertEvent[]> {
  if (IS_MOCK) {
    const all = (await loadMockAlerts()).mockEvents();
    return delay(status ? all.filter((e) => e.status === status) : all, 250);
  }
  const values = await apiGet<unknown>("/alerts", {
    base: API_ROOT,
    params: status ? { status } : undefined,
    signal,
    project,
  });
  return normalizeAlertEventList(values);
}

/** Severity of an event, derived from its `severity` label (defaults to info). */
export function eventSeverity(e: AlertEvent): Severity {
  const s = e.labels.severity;
  return s === "critical" || s === "warning" || s === "info" ? s : "info";
}
