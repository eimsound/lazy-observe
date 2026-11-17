import type { LogLevel } from "@/lib/api/logs";

const LEVEL_LABEL_KEY: Record<LogLevel, string> = {
  error: "logs.levelError",
  warn: "logs.levelWarn",
  info: "logs.levelInfo",
  debug: "logs.levelDebug",
  trace: "logs.levelTrace",
  unknown: "logs.levelUnknown",
};

export function levelLabelKey(level: LogLevel): string {
  return LEVEL_LABEL_KEY[level];
}
