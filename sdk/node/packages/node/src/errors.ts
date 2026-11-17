import { randomUUID } from "node:crypto";
import type { SpanContext } from "@opentelemetry/api";

export interface StackFrame {
  function: string;
  filename: string;
  lineno?: number;
  colno?: number;
  abs_path?: string;
  module?: string;
  in_app: boolean;
}

export interface ErrorEvent {
  event_id: string;
  timestamp: string;
  platform: "node";
  release?: string;
  environment?: string;
  level: "error" | "warning";
  message: string;
  exception: { type: string; value: string; stacktrace: { frames: StackFrame[] } };
  tags?: Record<string, string>;
  contexts?: { trace?: { trace_id: string; span_id: string } };
  url?: string;
}

const framePattern = /^\s*at\s+(?:(async)\s+)?(?:(.*?)\s+\()?((?:file:\/\/)?[^()]+?):(\d+):(\d+)\)?$/;

export function parseV8Stack(stack?: string): StackFrame[] {
  if (!stack) return [];
  const frames: StackFrame[] = [];
  for (const line of stack.split("\n").slice(1)) {
    const match = framePattern.exec(line);
    if (!match) continue;
    const rawFile = match[3]!.trim();
    const filename = rawFile.startsWith("file://") ? decodeURIComponent(new URL(rawFile).pathname) : rawFile;
    const fn = `${match[1] ? "async " : ""}${match[2] || "<anonymous>"}`;
    const moduleMatch = /node_modules\/(?:@([^/]+)\/)?([^/]+)/.exec(filename);
    frames.push({
      function: fn,
      filename,
      lineno: Number(match[4]),
      colno: Number(match[5]),
      abs_path: filename,
      ...(moduleMatch ? { module: moduleMatch[1] ? `@${moduleMatch[1]}/${moduleMatch[2]}` : moduleMatch[2] } : {}),
      in_app: !filename.includes("node_modules") && !filename.startsWith("node:") && !filename.startsWith("internal/"),
    });
  }
  return frames.reverse();
}

export function errorEvent(error: unknown, options: {
  spanContext?: SpanContext;
  release?: string;
  environment?: string;
  tags?: Record<string, string>;
  url?: string;
} = {}): ErrorEvent {
  const err = error instanceof Error ? error : new Error(String(error));
  const sc = options.spanContext;
  return {
    event_id: randomUUID(), timestamp: new Date().toISOString(), platform: "node", level: "error",
    message: err.message,
    exception: { type: err.name || "Error", value: err.message, stacktrace: { frames: parseV8Stack(err.stack) } },
    ...(options.release ? { release: options.release } : {}),
    ...(options.environment ? { environment: options.environment } : {}),
    ...(options.tags ? { tags: options.tags } : {}),
    ...(options.url ? { url: options.url } : {}),
    ...(sc?.traceId && sc.spanId ? { contexts: { trace: { trace_id: sc.traceId, span_id: sc.spanId } } } : {}),
  };
}
