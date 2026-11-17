import { useTranslation } from "react-i18next";
import { cn } from "@/lib/utils";
import type { Severity, AlertStatus } from "@/lib/api/alerts";
import { severityLabelKey } from "./severity-labels";

/**
 * Severity + status visuals for the alerting surface. Severity reuses the
 * palette-independent log-level tokens (--level-*) so "critical/warning/info"
 * stay consistent with the Logs page. No hardcoded colors.
 */

const SEVERITY_DOT: Record<Severity, string> = {
  critical: "bg-level-error",
  warning: "bg-level-warn",
  info: "bg-level-info",
};

const SEVERITY_CHIP: Record<Severity, string> = {
  critical: "border-level-error/30 text-level-error bg-level-error/10",
  warning: "border-level-warn/30 text-level-warn bg-level-warn/10",
  info: "border-level-info/30 text-level-info bg-level-info/10",
};

export function SeverityDot({ severity, className }: { severity: Severity; className?: string }) {
  return (
    <span
      className={cn("inline-block size-2 shrink-0 rounded-full", SEVERITY_DOT[severity], className)}
      aria-hidden
    />
  );
}

export function SeverityBadge({ severity }: { severity: Severity }) {
  const { t } = useTranslation();
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 rounded-md border px-2 py-0.5 text-xs font-medium",
        SEVERITY_CHIP[severity],
      )}
    >
      <SeverityDot severity={severity} />
      {t(severityLabelKey(severity))}
    </span>
  );
}

export function StatusBadge({ status }: { status: AlertStatus }) {
  const { t } = useTranslation();
  const firing = status === "firing";
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 rounded-md border px-2 py-0.5 text-xs font-medium",
        firing
          ? "border-level-error/30 bg-level-error/10 text-level-error"
          : "border-border bg-muted/50 text-muted-foreground",
      )}
    >
      <span
        className={cn(
          "inline-block size-1.5 rounded-full",
          firing ? "bg-level-error" : "bg-muted-foreground",
        )}
        aria-hidden
      />
      {t(firing ? "alerts.statusFiring" : "alerts.statusResolved")}
    </span>
  );
}

/** Small key=value chips used to render label sets. */
export function LabelChips({
  labels,
  max,
  exclude = [],
}: {
  labels: Record<string, string>;
  max?: number;
  exclude?: string[];
}) {
  const entries = Object.entries(labels).filter(([k]) => !exclude.includes(k));
  const shown = max ? entries.slice(0, max) : entries;
  const hidden = entries.length - shown.length;
  return (
    <div className="flex flex-wrap gap-1">
      {shown.map(([k, v]) => (
        <span
          key={k}
          className="inline-flex max-w-[220px] items-center gap-1 rounded border border-border bg-muted/40 px-1.5 py-0.5 font-mono text-[11px] text-muted-foreground"
        >
          <span className="truncate">
            <span className="text-foreground/70">{k}</span>
            <span className="opacity-60">=</span>
            {v}
          </span>
        </span>
      ))}
      {hidden > 0 && (
        <span className="inline-flex items-center rounded border border-border bg-muted/40 px-1.5 py-0.5 text-[11px] text-muted-foreground">
          +{hidden}
        </span>
      )}
    </div>
  );
}
