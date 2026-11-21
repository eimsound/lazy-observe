import {
  useCallback,
  useEffect,
  useRef,
  useState,
} from "react";
import type { LogRow } from "@/lib/api/logs";
import { formatDateTime } from "@/lib/format";
import {
  getRowField,
  safeJsonStringify,
  stringifyFieldValue,
} from "./logs-explorer-model";

export type LogsCopyHandler = (text: string) => void | Promise<void>;
export type LogsFieldActionHandler = (
  field: string,
  value: string,
  row: LogRow,
) => void;
export type LogsTraceActionHandler = (traceId: string, row: LogRow) => void;
export type LogsContextActionHandler = (row: LogRow) => void;

export interface CommonViewProps {
  rows: LogRow[];
  className?: string;
  /** Override clipboard handling, for example to add a toast or test the view. */
  onCopy?: LogsCopyHandler;
}

export function fieldActionValue(value: unknown): string {
  if (value === null) return "null";
  if (value === undefined) return "";
  if (typeof value === "string") return value;
  if (typeof value === "object") return safeJsonStringify(value);
  return String(value);
}

export function getDisplayValue(row: LogRow, field: string): string {
  if (field === "_time" && row.time > 0) return formatDateTime(row.time);
  return stringifyFieldValue(getRowField(row, field).value);
}

export async function writeClipboard(text: string): Promise<void> {
  if (typeof navigator !== "undefined" && navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(text);
    return;
  }
  if (typeof document === "undefined") throw new Error("Clipboard unavailable");

  const textarea = document.createElement("textarea");
  textarea.value = text;
  textarea.setAttribute("readonly", "");
  textarea.style.position = "fixed";
  textarea.style.opacity = "0";
  document.body.appendChild(textarea);
  textarea.select();
  const copied = document.execCommand("copy");
  textarea.remove();
  if (!copied) throw new Error("Clipboard unavailable");
}

export function useCopyFeedback(onCopy?: LogsCopyHandler) {
  const [copiedKey, setCopiedKey] = useState<string | null>(null);
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(
    () => () => {
      if (timerRef.current !== null) clearTimeout(timerRef.current);
    },
    [],
  );

  const copy = useCallback(
    async (text: string, key: string) => {
      try {
        await (onCopy ? onCopy(text) : writeClipboard(text));
        setCopiedKey(key);
        if (timerRef.current !== null) clearTimeout(timerRef.current);
        timerRef.current = setTimeout(() => setCopiedKey(null), 1600);
      } catch {
        setCopiedKey(null);
      }
    },
    [onCopy],
  );

  return { copiedKey, copy };
}
