import { useTranslation } from "react-i18next";
import type { LogLevel } from "@/lib/api/logs";
import { levelLabelKey } from "./level-labels";
import { cn } from "@/lib/utils";

/**
 * Maps a log level to its semantic color token (defined in globals.css as
 * --level-*) and i18n label. No hardcoded colors — dots use bg-level-*.
 */

const LEVEL_DOT: Record<LogLevel, string> = {
  error: "bg-level-error",
  warn: "bg-level-warn",
  info: "bg-level-info",
  debug: "bg-level-debug",
  trace: "bg-level-trace",
  unknown: "bg-muted-foreground",
};

const LEVEL_TEXT: Record<LogLevel, string> = {
  error: "text-level-error",
  warn: "text-level-warn",
  info: "text-level-info",
  debug: "text-level-debug",
  trace: "text-level-trace",
  unknown: "text-muted-foreground",
};

/** A small colored dot for the log row. */
export function LevelDot({ level, className }: { level: LogLevel; className?: string }) {
  return (
    <span
      className={cn("inline-block size-2 shrink-0 rounded-full", LEVEL_DOT[level], className)}
      aria-hidden
    />
  );
}

/** Dot + textual level label (used in the detail drawer / column). */
export function LevelBadge({ level }: { level: LogLevel }) {
  const { t } = useTranslation();
  return (
    <span className={cn("inline-flex items-center gap-1.5 text-xs font-medium", LEVEL_TEXT[level])}>
      <LevelDot level={level} />
      {t(levelLabelKey(level))}
    </span>
  );
}
