import type { StackFrame } from "./types";

const EXTENSION = /^(?:chrome|moz|safari-web)-extension:/i;

export function isInApp(filename: string, origin?: string): boolean {
  if (!filename || filename === "<anonymous>" || filename.startsWith("eval")) return true;
  if (EXTENSION.test(filename)) return false;
  try {
    const base = origin ?? (typeof location === "undefined" ? undefined : location.origin);
    if (!base) return !/^https?:/i.test(filename);
    return new URL(filename, base).origin === base;
  } catch { return false; }
}

function numeric(value?: string): number | undefined {
  if (!value) return undefined;
  const parsed = Number(value);
  return Number.isFinite(parsed) ? parsed : undefined;
}

function parseLine(line: string, origin?: string): StackFrame | undefined {
  const trimmed = line.trim();
  if (!trimmed || /^\w*Error\b/.test(trimmed)) return undefined;
  let fn = "<anonymous>";
  let location = "";
  let match = trimmed.match(/^at\s+(.*?)\s+\((.*)\)$/);
  if (match) [, fn, location] = match;
  else if ((match = trimmed.match(/^at\s+(.*)$/))) location = match[1];
  else if ((match = trimmed.match(/^(.*?)@(.*)$/))) [, fn, location] = match;
  else return undefined;

  // Keep the final URL inside Chrome eval frames; line/column always come last.
  const loc = location.match(/^(.*?)(?::(\d+))?(?::(\d+))$/);
  if (!loc) return undefined;
  let filename = loc[1].replace(/^async\s+/, "");
  if (filename.includes(", ")) filename = filename.slice(filename.lastIndexOf(", ") + 2);
  if (!filename) filename = "<anonymous>";
  return {
    function: fn || "<anonymous>",
    filename,
    lineno: numeric(loc[2]),
    colno: numeric(loc[3]),
    in_app: isInApp(filename, origin)
  };
}

export function parseStack(stack?: string, origin?: string): StackFrame[] {
  if (!stack) return [];
  return stack.split("\n").map((line) => parseLine(line, origin)).filter((frame): frame is StackFrame => Boolean(frame)).reverse();
}
