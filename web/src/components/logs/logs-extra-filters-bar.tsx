import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { Check, Copy, X } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  buildLogsExtraFilterParams,
  logsFilterLabel,
  type LogsExtraFilter,
} from "@/lib/logs-explorer";
import { cn } from "@/lib/utils";

export interface ExtraFiltersBarProps {
  filters: LogsExtraFilter[];
  onRemoveFilter: (field: string, value: string) => void;
  onClearFilters: () => void;
  className?: string;
}

function formatFilterLabel(filter: LogsExtraFilter): string {
  const label = logsFilterLabel(filter);
  return filter.isStream ? `{${label}}` : label;
}

function compileFilters(filters: readonly LogsExtraFilter[]): string {
  const compiled = buildLogsExtraFilterParams(filters);
  return [...compiled.extraStreamFilters, ...compiled.extraFilters]
    .map((expression) => `(${expression})`)
    .join(" AND ");
}

async function copyText(value: string): Promise<boolean> {
  if (navigator.clipboard) {
    try {
      await navigator.clipboard.writeText(value);
      return true;
    } catch {
      // Clipboard API can be unavailable on non-secure LAN origins; use the DOM fallback.
    }
  }

  const textarea = document.createElement("textarea");
  textarea.value = value;
  textarea.setAttribute("readonly", "");
  textarea.style.position = "fixed";
  textarea.style.opacity = "0";
  document.body.appendChild(textarea);
  textarea.select();
  try {
    return document.execCommand("copy");
  } finally {
    textarea.remove();
  }
}

export function ExtraFiltersBar({
  filters,
  onRemoveFilter,
  onClearFilters,
  className,
}: ExtraFiltersBarProps) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);
  const expression = useMemo(() => compileFilters(filters), [filters]);

  useEffect(() => {
    setCopied(false);
  }, [expression]);

  if (!filters.length) return null;

  const copyExpression = async () => {
    if (!expression) return;
    try {
      setCopied(await copyText(expression));
    } catch {
      setCopied(false);
    }
  };

  return (
    <div
      className={cn(
        "flex min-w-0 flex-wrap items-center gap-1.5 rounded-md border border-border bg-muted/30 p-2",
        className,
      )}
      aria-label={t("logs.activeFilters")}
    >
      {filters.map((filter, index) => {
        const label = formatFilterLabel(filter);
        return (
          <span
            key={`${filter.field}\u0000${filter.value}\u0000${filter.operator}\u0000${index}`}
            className="inline-flex h-7 max-w-full items-center rounded-md border border-border bg-background text-xs shadow-sm"
          >
            <span className="max-w-64 truncate px-2 font-mono" title={label}>
              {label}
            </span>
            <button
              type="button"
              className="flex h-full items-center border-l border-border px-1.5 text-muted-foreground transition-colors hover:bg-accent hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
              aria-label={t("logs.removeFilter", { filter: label })}
              onClick={() => onRemoveFilter(filter.field, filter.value)}
            >
              <X className="size-3.5" aria-hidden="true" />
            </button>
          </span>
        );
      })}

      <div className="ml-auto flex items-center gap-1">
        <Button
          type="button"
          variant="ghost"
          size="sm"
          className="h-7 px-2"
          aria-label={copied ? t("logs.filtersCopied") : t("logs.copyFilters")}
          onClick={() => void copyExpression()}
        >
          {copied ? (
            <Check className="size-3.5" aria-hidden="true" />
          ) : (
            <Copy className="size-3.5" aria-hidden="true" />
          )}
          <span className="hidden sm:inline">
            {copied ? t("logs.filtersCopied") : t("logs.copyFilters")}
          </span>
        </Button>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          className="h-7 px-2 text-muted-foreground"
          onClick={onClearFilters}
        >
          {t("logs.clearFilters")}
        </Button>
      </div>
      <span className="sr-only" aria-live="polite">
        {copied ? t("logs.filtersCopied") : ""}
      </span>
    </div>
  );
}
