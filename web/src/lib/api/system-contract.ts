const MANAGED_COMPONENTS = ["gateway", "metrics", "logs", "traces", "vmagent", "error_inbox"] as const;

export type ManagedComponentName = (typeof MANAGED_COMPONENTS)[number];
export type ManagedComponentState = "ok" | "restarting" | "unavailable";

export interface HealthDiagnostics {
  status: "ok" | "degraded";
  components: Array<{ name: ManagedComponentName; state: ManagedComponentState }>;
}

export interface InboxDiagnostics {
  pendingItems: number;
  processingItems: number;
  activeItems: number;
  activeBytes: number;
  oldestAcceptedAt: string | null;
  retriesTotal: number;
  rejectionsTotal: number;
  terminalFailuresTotal: number;
  dropsTotal: number;
}

function record(value: unknown, label: string): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new TypeError(`${label} must be an object`);
  }
  return value as Record<string, unknown>;
}

function counter(value: unknown, label: string): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 0) {
    throw new TypeError(`${label} must be a non-negative integer`);
  }
  return value;
}

const HEALTHY_ERROR_INBOX_SUMMARY = /^active_items=\d+\s+active_bytes=\d+$/;

function normalizeComponentState(
  name: ManagedComponentName,
  rawState: string,
): ManagedComponentState {
  const trimmed = rawState.trim();
  if (trimmed === "ok") return "ok";
  if (name === "error_inbox" && HEALTHY_ERROR_INBOX_SUMMARY.test(trimmed)) {
    return "ok";
  }
  if (trimmed.toLowerCase().includes("restarting")) return "restarting";
  return "unavailable";
}

export function normalizeHealthDiagnostics(value: unknown): HealthDiagnostics {
  const envelope = record(value, "health diagnostics");
  const rawComponents = record(envelope.components, "health diagnostics components");
  const components = MANAGED_COMPONENTS.flatMap((name) => {
    const rawState = rawComponents[name];
    if (typeof rawState !== "string") return [];
    return [{ name, state: normalizeComponentState(name, rawState) }];
  });
  if (components.length === 0) throw new TypeError("health diagnostics has no known components");
  return {
    status: envelope.status === "ok" ? "ok" : "degraded",
    components,
  };
}

export function normalizeInboxDiagnostics(value: unknown): InboxDiagnostics {
  const diagnostics = record(value, "Inbox diagnostics");
  const oldest = diagnostics.oldest_accepted_at;
  if (oldest !== undefined && oldest !== null && typeof oldest !== "string") {
    throw new TypeError("oldest_accepted_at must be a string or null");
  }
  return {
    pendingItems: counter(diagnostics.pending_items, "pending_items"),
    processingItems: counter(diagnostics.processing_items, "processing_items"),
    activeItems: counter(diagnostics.active_items, "active_items"),
    activeBytes: counter(diagnostics.active_bytes, "active_bytes"),
    oldestAcceptedAt: oldest ?? null,
    retriesTotal: counter(diagnostics.retries_total, "retries_total"),
    rejectionsTotal: counter(diagnostics.rejections_total, "rejections_total"),
    terminalFailuresTotal: counter(diagnostics.terminal_failures_total, "terminal_failures_total"),
    dropsTotal: counter(diagnostics.drops_total, "drops_total"),
  };
}
