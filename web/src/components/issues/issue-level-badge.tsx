import { useTranslation } from "react-i18next";
import type { IssueLevel } from "@/lib/api/issues";
import { cn } from "@/lib/utils";

/**
 * Issue severity badge. Colors come from the palette-independent `--level-*`
 * semantic tokens (same fixed meaning as log levels), never a hardcoded hex.
 */

const LEVEL_STYLES: Record<IssueLevel, string> = {
  error: "border-level-error/30 bg-level-error/10 text-level-error",
  warning: "border-level-warn/30 bg-level-warn/10 text-level-warn",
  info: "border-level-info/30 bg-level-info/10 text-level-info",
};

const LEVEL_LABEL_KEY: Record<IssueLevel, string> = {
  error: "issues.levelError",
  warning: "issues.levelWarning",
  info: "issues.levelInfo",
};

export function IssueLevelBadge({ level, className }: { level: IssueLevel; className?: string }) {
  const { t } = useTranslation();
  return (
    <span
      className={cn(
        "inline-flex items-center rounded-md border px-1.5 py-0.5 text-xs font-medium capitalize",
        LEVEL_STYLES[level],
        className,
      )}
    >
      {t(LEVEL_LABEL_KEY[level])}
    </span>
  );
}

/** A small level-colored dot (for compact rows / markers). */
export function IssueLevelDot({ level, className }: { level: IssueLevel; className?: string }) {
  const color =
    level === "error" ? "var(--level-error)" : level === "warning" ? "var(--level-warn)" : "var(--level-info)";
  return (
    <span
      className={cn("inline-block size-2 shrink-0 rounded-full", className)}
      style={{ backgroundColor: color }}
    />
  );
}
