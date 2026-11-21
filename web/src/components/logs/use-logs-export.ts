import { useCallback, useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { exportLogs } from "@/lib/api/logs";
import type { LogsExtraFilterParams } from "@/lib/logs-explorer";
import type { ExecutedLogsQuery } from "@/routes/logs-workspace";

interface LogsFileHandle {
  createWritable: () => Promise<WritableStream<Uint8Array>>;
}

interface LogsFilePickerWindow extends Window {
  showSaveFilePicker?: (options: {
    suggestedName: string;
    types: Array<{
      description: string;
      accept: Record<string, string[]>;
    }>;
  }) => Promise<LogsFileHandle>;
}

interface UseLogsExportOptions {
  executed: ExecutedLogsQuery | null;
  effectiveRange: { start: number; end: number } | null;
  filtersForRequest: LogsExtraFilterParams;
}

function isAbortError(error: unknown): boolean {
  return error instanceof DOMException && error.name === "AbortError";
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

export function useLogsExport({
  executed,
  effectiveRange,
  filtersForRequest,
}: UseLogsExportOptions) {
  const { t } = useTranslation();
  const [downloading, setDownloading] = useState(false);
  const [downloadError, setDownloadError] = useState("");
  const downloadController = useRef<AbortController | null>(null);

  useEffect(() => () => downloadController.current?.abort(), []);

  const download = useCallback(async (
    format: "jsonl" | "csv",
    filenameBase: string,
  ) => {
    if (!executed || !effectiveRange) return;
    setDownloadError("");
    const filename = `${filenameBase}.${format}`;
    const picker = (window as LogsFilePickerWindow).showSaveFilePicker;
    if (!picker && !window.confirm(t("logs.downloadFallbackConfirm"))) return;
    const controller = new AbortController();
    downloadController.current?.abort();
    downloadController.current = controller;
    setDownloading(true);
    let writable: WritableStream<Uint8Array> | null = null;
    try {
      if (picker) {
        const handle = await picker.call(window, {
          suggestedName: filename,
          types: [{
            description: format === "csv" ? "CSV" : "JSON Lines",
            accept: format === "csv"
              ? { "text/csv": [".csv"] }
              : { "application/x-ndjson": [".jsonl"] },
          }],
        });
        writable = await handle.createWritable();
      }
      const response = await exportLogs({
        query: executed.query,
        start: effectiveRange.start,
        end: effectiveRange.end,
        format,
        project: executed.project,
        ...filtersForRequest,
      }, controller.signal);
      if (writable) {
        if (!response.body) throw new Error(t("logs.downloadMissingBody"));
        await response.body.pipeTo(writable, { signal: controller.signal });
        writable = null;
      } else {
        const blob = await response.blob();
        const url = URL.createObjectURL(blob);
        const anchor = document.createElement("a");
        anchor.href = url;
        anchor.download = filename;
        document.body.appendChild(anchor);
        anchor.click();
        anchor.remove();
        URL.revokeObjectURL(url);
      }
    } catch (error) {
      if (writable && !writable.locked) {
        await writable.abort(error).catch(() => undefined);
      }
      if (!isAbortError(error)) setDownloadError(errorMessage(error));
    } finally {
      if (downloadController.current === controller) {
        downloadController.current = null;
        setDownloading(false);
      }
    }
  }, [effectiveRange, executed, filtersForRequest, t]);

  const cancelDownload = useCallback(() => {
    downloadController.current?.abort();
  }, []);

  return {
    downloading,
    downloadError,
    download,
    cancelDownload,
  };
}
