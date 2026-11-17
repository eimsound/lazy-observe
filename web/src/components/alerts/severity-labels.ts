import type { Severity } from "@/lib/api/alerts";

const SEVERITY_LABEL: Record<Severity, string> = {
  critical: "alerts.sevCritical",
  warning: "alerts.sevWarning",
  info: "alerts.sevInfo",
};

export function severityLabelKey(sev: Severity): string {
  return SEVERITY_LABEL[sev];
}
