export interface AlertConfig {
  new_issue: boolean;
  regression: boolean;
  error_rate_threshold_per_min: number;
  channel_ids: string[];
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

export function normalizeAlertConfigResponse(value: unknown): AlertConfig {
  if (!isRecord(value)) throw new TypeError("alert config must be an object");
  if (typeof value.new_issue !== "boolean" || typeof value.regression !== "boolean") {
    throw new TypeError("alert config switches must be booleans");
  }
  const threshold = value.error_rate_threshold_per_min;
  if (typeof threshold !== "number" || !Number.isSafeInteger(threshold) || threshold < 0) {
    throw new TypeError("alert config threshold must be a non-negative integer");
  }
  if (!Array.isArray(value.channel_ids) || !value.channel_ids.every((id) => typeof id === "string" && id.length > 0)) {
    throw new TypeError("alert config.channel_ids must contain strings");
  }
  return {
    new_issue: value.new_issue,
    regression: value.regression,
    error_rate_threshold_per_min: threshold,
    channel_ids: [...value.channel_ids],
  };
}
