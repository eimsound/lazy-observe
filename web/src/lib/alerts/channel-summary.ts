import type { NotificationChannel, ChannelType } from "@/lib/api/alerts";

export const REDACTED_SENTINEL = "[REDACTED]";

export interface ChannelFieldSpec {
  key: string;
  labelKey: string;
  required?: boolean;
  password?: boolean;
}

export const CHANNEL_FIELDS: Record<ChannelType, ChannelFieldSpec[]> = {
  webhook: [
    { key: "url", labelKey: "alerts.cfgUrl", required: true },
    { key: "secret", labelKey: "alerts.cfgSecret", password: true },
  ],
  feishu: [{ key: "url", labelKey: "alerts.cfgWebhookUrl", required: true }],
  dingtalk: [
    { key: "url", labelKey: "alerts.cfgWebhookUrl", required: true },
    { key: "secret", labelKey: "alerts.cfgSecret", password: true },
  ],
  wecom: [{ key: "url", labelKey: "alerts.cfgWebhookUrl", required: true }],
  slack: [{ key: "url", labelKey: "alerts.cfgWebhookUrl", required: true }],
  smtp: [
    { key: "to", labelKey: "alerts.cfgTo", required: true },
    { key: "from", labelKey: "alerts.cfgFrom", required: true },
    { key: "host", labelKey: "alerts.cfgSmtpHost", required: true },
    { key: "port", labelKey: "alerts.cfgSmtpPort", required: true },
    { key: "username", labelKey: "alerts.cfgUsername" },
    { key: "password", labelKey: "alerts.cfgPassword", password: true },
  ],
};

const SENSITIVE_QUERY_MARKERS = [
  "token",
  "secret",
  "key",
  "sign",
  "password",
  "passwd",
  "auth",
  "credential",
];

function isSensitiveQueryParam(name: string): boolean {
  const lower = name.toLowerCase();
  return SENSITIVE_QUERY_MARKERS.some((marker) => lower.includes(marker));
}

/**
 * Redacts userinfo and sensitive query values in a URL before displaying it on
 * a channel card. Never throws on malformed input.
 */
export function sanitizeDisplayUrl(raw: string): string {
  const trimmed = raw.trim();
  if (!trimmed) return "";
  if (trimmed === REDACTED_SENTINEL) return REDACTED_SENTINEL;
  try {
    const parsed = new URL(trimmed);
    parsed.username = "";
    parsed.password = "";
    for (const [key] of parsed.searchParams) {
      if (isSensitiveQueryParam(key)) {
        parsed.searchParams.set(key, REDACTED_SENTINEL);
      }
    }
    return parsed.toString();
  } catch {
    return trimmed;
  }
}

/**
 * Renders a non-sensitive summary line for a notification channel card using an
 * explicit per-type field allowlist. Never iterates `Object.values(ch.config)`
 * so signing secrets and SMTP passwords cannot leak onto the card.
 */
export function formatChannelSummary(
  channel: Pick<NotificationChannel, "type" | "config">,
): string {
  const cfg = channel.config ?? {};
  if (channel.type === "smtp") {
    const to = (cfg.to ?? "").trim();
    const host = (cfg.host ?? "").trim();
    const port = (cfg.port ?? "").trim();
    const endpoint = host ? (port ? `${host}:${port}` : host) : "";
    if (to && endpoint) return `${to} (${endpoint})`;
    if (to) return to;
    if (endpoint) return endpoint;
    return "—";
  }

  const url = sanitizeDisplayUrl(cfg.url ?? "");
  return url || "—";
}

/**
 * Builds initial form state when editing a channel: keeps non-secret fields
 * editable and tracks which secret/password fields already have a stored value
 * on the server without populating plaintext or `[REDACTED]` into inputs.
 */
export function initChannelFormConfig(
  editing: Pick<NotificationChannel, "type" | "config"> | null,
): { values: Record<string, string>; storedSecrets: Record<string, boolean> } {
  if (!editing) return { values: {}, storedSecrets: {} };
  const fields = CHANNEL_FIELDS[editing.type] ?? [];
  const values: Record<string, string> = {};
  const storedSecrets: Record<string, boolean> = {};
  for (const field of fields) {
    const raw = (editing.config[field.key] ?? "").trim();
    if (field.password) {
      if (raw !== "") {
        storedSecrets[field.key] = true;
      }
      values[field.key] = "";
    } else {
      values[field.key] = editing.config[field.key] ?? "";
    }
  }
  return { values, storedSecrets };
}

/**
 * Builds the config payload sent on create/update. When editing, an empty
 * password/secret field that already had a stored server secret sends
 * `[REDACTED]` so the gateway preserves the existing encrypted credential.
 */
export function buildChannelConfigPayload(
  type: ChannelType,
  values: Record<string, string>,
  storedSecrets: Record<string, boolean>,
  isEditing: boolean,
): Record<string, string> {
  const fields = CHANNEL_FIELDS[type] ?? [];
  const cleanConfig: Record<string, string> = {};
  for (const field of fields) {
    const v = (values[field.key] ?? "").trim();
    if (v) {
      cleanConfig[field.key] = v;
    } else if (isEditing && field.password && storedSecrets[field.key]) {
      cleanConfig[field.key] = REDACTED_SENTINEL;
    }
  }
  return cleanConfig;
}
