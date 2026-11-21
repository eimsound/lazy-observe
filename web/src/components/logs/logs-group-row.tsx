import {
  Check,
  ChevronDown,
  ChevronUp,
  Copy,
  Eye,
  Filter,
  FilterX,
  ListTree,
  PanelTopOpen,
  Search,
  Waypoints,
} from "lucide-react";
import { useId, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { Input } from "@/components/ui/input";
import type { LogRow } from "@/lib/api/logs";
import { formatDateTime } from "@/lib/format";
import { cn } from "@/lib/utils";
import { LevelBadge } from "./level-indicator";
import { renderAnsiMessage } from "./log-message-renderers";
import {
  getRowField,
  safeJsonStringify,
  stringifyFieldValue,
} from "./logs-explorer-model";
import { IconAction } from "./logs-explorer-shared";
import {
  fieldActionValue,
  useCopyFeedback,
  type LogsContextActionHandler,
  type LogsCopyHandler,
  type LogsFieldActionHandler,
  type LogsTraceActionHandler,
} from "./logs-explorer-utils";
import { SafeMarkdownMessage } from "./safe-markdown-message";

export interface ExpandableLogRowProps {
  row: LogRow;
  displayFields: string[];
  singleLine: boolean;
  compact: boolean;
  markdownParsing: boolean;
  ansiParsing: boolean;
  disableHover: boolean;
  onIncludeField: LogsFieldActionHandler;
  onExcludeField: LogsFieldActionHandler;
  onSetGroupBy: (field: string) => void;
  onAddDisplayField: (field: string) => void;
  onViewTrace: LogsTraceActionHandler;
  onShowContext?: LogsContextActionHandler;
  onCopy?: LogsCopyHandler;
}

export function ExpandableLogRow({
  row,
  displayFields,
  singleLine,
  compact,
  markdownParsing,
  ansiParsing,
  disableHover,
  onIncludeField,
  onExcludeField,
  onSetGroupBy,
  onAddDisplayField,
  onViewTrace,
  onShowContext,
  onCopy,
}: ExpandableLogRowProps) {
  const { t } = useTranslation();
  const [expanded, setExpanded] = useState(false);
  const [fieldSearch, setFieldSearch] = useState("");
  const detailsId = useId();
  const { copiedKey, copy } = useCopyFeedback(onCopy);
  const rawJson = useMemo(() => safeJsonStringify(row.raw), [row.raw]);
  const ansiMessage = useMemo(
    () => (ansiParsing ? renderAnsiMessage(row.message) : null),
    [ansiParsing, row.message],
  );
  const hasContextIdentity =
    Object.prototype.hasOwnProperty.call(row.raw, "_stream_id") &&
    Object.prototype.hasOwnProperty.call(row.raw, "_time");

  const matchingFields = useMemo(() => {
    const query = fieldSearch.trim().toLocaleLowerCase();
    return Object.entries(row.raw)
      .sort(([left], [right]) => left.localeCompare(right))
      .filter(([field, value]) => {
        if (!query) return true;
        return (
          field.toLocaleLowerCase().includes(query) ||
          stringifyFieldValue(value).toLocaleLowerCase().includes(query)
        );
      });
  }, [fieldSearch, row.raw]);

  return (
    <div className="bg-background/70">
      <div
        className={cn(
          "group/row flex items-start gap-1",
          !disableHover && "hover:bg-muted/40",
          compact ? "px-2 py-1" : "px-3 py-2",
        )}
      >
        <div
          className="flex min-w-0 flex-1 cursor-pointer items-start gap-2 rounded-sm text-left"
          onClick={() => setExpanded((current) => !current)}
        >
          <button
            type="button"
            className="mt-0.5 shrink-0 rounded-sm text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
            aria-label={t(expanded ? "logs.collapseRow" : "logs.expandRow")}
            aria-expanded={expanded}
            aria-controls={detailsId}
          >
            {expanded ? (
              <ChevronUp className="size-3.5" />
            ) : (
              <ChevronDown className="size-3.5" />
            )}
          </button>
          <span className="w-[142px] shrink-0 font-mono text-xs tabular-nums text-muted-foreground">
            {formatDateTime(row.time)}
          </span>
          <span className="hidden w-20 shrink-0 sm:inline-flex">
            <LevelBadge level={row.level} />
          </span>
          <div className="min-w-0 flex-1">
            <div
              className={cn(
                "block font-mono text-xs text-foreground",
                singleLine ? "truncate" : "whitespace-pre-wrap break-words",
              )}
              title={singleLine ? row.message : undefined}
            >
              {!row.message ? (
                "\u00a0"
              ) : markdownParsing ? (
                <SafeMarkdownMessage
                  source={row.message}
                  singleLine={singleLine}
                />
              ) : ansiParsing ? (
                ansiMessage
              ) : (
                row.message || "\u00a0"
              )}
            </div>
            {displayFields.length > 0 && (
              <div className="mt-1 flex flex-wrap gap-1">
                {displayFields.map((field) => {
                  const fieldValue = getRowField(row, field);
                  return (
                    <span
                      key={field}
                      className="inline-flex min-w-0 max-w-72 rounded bg-muted px-1.5 py-0.5 font-mono text-[11px] text-muted-foreground"
                      title={`${field}=${stringifyFieldValue(fieldValue.value)}`}
                    >
                      <span className="shrink-0">{field}=</span>
                      <span className="truncate">
                        {fieldValue.present
                          ? stringifyFieldValue(fieldValue.value)
                          : t("logs.fieldMissing")}
                      </span>
                    </span>
                  );
                })}
              </div>
            )}
          </div>
        </div>

        <div
          className={cn(
            "flex shrink-0 items-center focus-within:opacity-100",
            disableHover
              ? "opacity-100"
              : "opacity-70 transition-opacity group-hover/row:opacity-100",
          )}
        >
          {onShowContext && hasContextIdentity && (
            <IconAction
              label={t("logs.showContext")}
              onClick={() => onShowContext(row)}
            >
              <PanelTopOpen />
            </IconAction>
          )}
          <IconAction
            label={
              copiedKey === "row" ? t("common.copied") : t("logs.copyRow")
            }
            onClick={() => void copy(rawJson, "row")}
          >
            {copiedKey === "row" ? <Check /> : <Copy />}
          </IconAction>
          {row.traceId && (
            <IconAction
              label={t("logs.viewTrace")}
              onClick={() => onViewTrace(row.traceId!, row)}
            >
              <Waypoints />
            </IconAction>
          )}
        </div>
      </div>

      {expanded && (
        <div
          id={detailsId}
          className="space-y-2 border-t border-border/50 bg-muted/20 px-3 py-2"
        >
          <div className="relative max-w-md">
            <Search
              className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground"
              aria-hidden
            />
            <Input
              type="search"
              value={fieldSearch}
              className="h-8 pl-8 text-xs"
              aria-label={t("logs.fieldSearch")}
              placeholder={t("logs.fieldSearchPlaceholder")}
              onChange={(event) => setFieldSearch(event.target.value)}
            />
          </div>

          <div className="overflow-x-auto rounded border border-border/60 bg-background">
            <table className="w-full min-w-[42rem] border-collapse text-xs">
              <thead>
                <tr className="border-b border-border/70 bg-muted/50 text-left text-muted-foreground">
                  <th scope="col" className="w-[28%] px-2 py-1.5 font-medium">
                    {t("logs.fieldCol")}
                  </th>
                  <th scope="col" className="px-2 py-1.5 font-medium">
                    {t("logs.valueCol")}
                  </th>
                  <th
                    scope="col"
                    className="w-44 px-2 py-1.5 text-right font-medium"
                  >
                    {t("common.actions")}
                  </th>
                </tr>
              </thead>
              <tbody>
                {matchingFields.map(([field, value]) => {
                  const displayValue = stringifyFieldValue(value);
                  const actionValue = fieldActionValue(value);
                  const copyKey = `field:${field}`;
                  return (
                    <tr
                      key={field}
                      className={cn(
                        "border-b border-border/50 align-top last:border-0",
                        !disableHover && "hover:bg-muted/30",
                      )}
                    >
                      <th
                        scope="row"
                        className="px-2 py-1.5 text-left font-mono font-medium text-muted-foreground"
                      >
                        {field}
                      </th>
                      <td className="max-w-xl break-all px-2 py-1.5 font-mono text-foreground">
                        {displayValue}
                      </td>
                      <td className="px-2 py-1 text-right">
                        <div className="flex justify-end gap-0.5">
                          <IconAction
                            label={
                              copiedKey === copyKey
                                ? t("common.copied")
                                : t("common.copy")
                            }
                            onClick={() =>
                              void copy(actionValue, copyKey)
                            }
                          >
                            {copiedKey === copyKey ? <Check /> : <Copy />}
                          </IconAction>
                          <IconAction
                            label={t("logs.includeField")}
                            onClick={() =>
                              onIncludeField(field, actionValue, row)
                            }
                          >
                            <Filter />
                          </IconAction>
                          <IconAction
                            label={t("logs.excludeField")}
                            onClick={() =>
                              onExcludeField(field, actionValue, row)
                            }
                          >
                            <FilterX />
                          </IconAction>
                          <IconAction
                            label={t("logs.groupByField")}
                            onClick={() => onSetGroupBy(field)}
                          >
                            <ListTree />
                          </IconAction>
                          <IconAction
                            label={
                              displayFields.includes(field)
                                ? t("logs.fieldAlreadyShown")
                                : t("logs.showField")
                            }
                            disabled={displayFields.includes(field)}
                            onClick={() => onAddDisplayField(field)}
                          >
                            <Eye />
                          </IconAction>
                        </div>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
            {matchingFields.length === 0 && (
              <div className="p-4 text-center text-xs text-muted-foreground">
                {t("logs.noMatchingFields")}
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  );
}
