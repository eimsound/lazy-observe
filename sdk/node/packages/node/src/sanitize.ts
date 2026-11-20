import type { ErrorEvent } from "./errors.js";

export const MAX_ERROR_EVENT_BYTES: number = 200 * 1024;
export const MAX_MESSAGE_LENGTH: number = 8_192;
export const MAX_URL_LENGTH: number = 2_048;
export const MAX_TAG_VALUE_LENGTH: number = 4_096;
export const MAX_STACK_FRAMES: number = 50;
export const MAX_FRAME_FILENAME_LENGTH: number = 512;
export const MAX_FRAME_FUNCTION_LENGTH: number = 256;

const ABSOLUTE_HTTP_URL = /^https?:\/\//i;

/**
 * Strip credentials, query string, and fragment from URLs or request paths
 * so secrets in query params or basic auth never leak into error events.
 */
export function sanitizeUrl(value: string): string {
  const trimmed = value.trim();
  if (!trimmed) return "";
  if (ABSOLUTE_HTTP_URL.test(trimmed)) {
    try {
      const parsed = new URL(trimmed);
      return `${parsed.origin}${parsed.pathname}`;
    } catch {
      return stripFallbackUrl(trimmed);
    }
  }
  if (/^[a-z][a-z0-9+.-]*:\/\//i.test(trimmed)) {
    return stripFallbackUrl(trimmed);
  }
  return trimmed.split(/[?#]/, 1)[0] ?? "";
}

function stripFallbackUrl(raw: string): string {
  const withoutQuery = raw.split(/[?#]/, 1)[0] ?? "";
  const schemeEnd = withoutQuery.indexOf("://");
  if (schemeEnd < 0) return withoutQuery;
  const afterScheme = withoutQuery.slice(schemeEnd + 3);
  const slashIdx = afterScheme.indexOf("/");
  const authority = slashIdx >= 0 ? afterScheme.slice(0, slashIdx) : afterScheme;
  const pathPart = slashIdx >= 0 ? afterScheme.slice(slashIdx) : "";
  const atIdx = authority.lastIndexOf("@");
  const cleanAuthority = atIdx >= 0 ? authority.slice(atIdx + 1) : authority;
  return `${withoutQuery.slice(0, schemeEnd + 3)}${cleanAuthority}${pathPart}`;
}

export function truncateText(value: string, maxChars: number): string {
  return value.length > maxChars ? `${value.slice(0, maxChars)}...[truncated]` : value;
}

export function estimateJsonBytes(value: unknown): number {
  return Buffer.byteLength(JSON.stringify(value), "utf8");
}

function sanitizeFramePath(value: string): string {
  const cleaned = ABSOLUTE_HTTP_URL.test(value) ? sanitizeUrl(value) : value;
  return truncateText(cleaned, MAX_FRAME_FILENAME_LENGTH);
}

/**
 * Unified privacy and size-budget sanitization pipeline for Node error events:
 * 1. Strip credentials, ?query, and #hash from event URL and HTTP(S) stack frame URLs.
 * 2. Cap message, exception value, tags, stack frames, and frame text fields.
 * 3. Enforce the single-event byte budget (default 200 KiB), progressively trimming or
 *    returning undefined if the event still cannot fit.
 */
export function sanitizeErrorEvent(
  event: ErrorEvent,
  maxBytes: number = MAX_ERROR_EVENT_BYTES,
): ErrorEvent | undefined {
  const sanitized: ErrorEvent = {
    ...event,
    message: truncateText(event.message, MAX_MESSAGE_LENGTH),
    exception: {
      ...event.exception,
      value: truncateText(event.exception.value, MAX_MESSAGE_LENGTH),
      stacktrace: {
        frames: event.exception.stacktrace.frames.slice(-MAX_STACK_FRAMES).map((frame) => ({
          ...frame,
          function: truncateText(frame.function, MAX_FRAME_FUNCTION_LENGTH),
          filename: sanitizeFramePath(frame.filename),
          ...(frame.abs_path !== undefined ? { abs_path: sanitizeFramePath(frame.abs_path) } : {}),
        })),
      },
    },
    ...(event.tags ? { tags: sanitizeTags(event.tags, MAX_TAG_VALUE_LENGTH) } : {}),
    ...(event.url !== undefined ? { url: truncateText(sanitizeUrl(event.url), MAX_URL_LENGTH) } : {}),
  };

  const fits = (): boolean => estimateJsonBytes(sanitized) <= maxBytes;
  if (fits()) return sanitized;

  if (sanitized.tags) {
    sanitized.tags = sanitizeTags(sanitized.tags, 512);
    if (fits()) return sanitized;
    delete sanitized.tags;
    if (fits()) return sanitized;
  }

  sanitized.message = truncateText(sanitized.message, 1_024);
  sanitized.exception.value = truncateText(sanitized.exception.value, 1_024);
  if (fits()) return sanitized;

  sanitized.exception.stacktrace.frames = sanitized.exception.stacktrace.frames.slice(-10);
  if (fits()) return sanitized;

  sanitized.exception.stacktrace.frames = [];
  return fits() ? sanitized : undefined;
}

function sanitizeTags(tags: Record<string, string>, maxLen: number): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [key, value] of Object.entries(tags)) {
    out[key] = truncateText(value, maxLen);
  }
  return out;
}

export function trimErrorEventToBudget(
  event: ErrorEvent,
  maxBytes: number = MAX_ERROR_EVENT_BYTES,
): ErrorEvent | undefined {
  return sanitizeErrorEvent(event, maxBytes);
}
