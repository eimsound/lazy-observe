import {
  ArrowDown,
  ArrowUp,
  Check,
  ChevronsUpDown,
  Columns3,
  Copy,
  GripVertical,
  MoreHorizontal,
  Waypoints,
} from "lucide-react";
import { useMemo } from "react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import type { LogRow } from "@/lib/api/logs";
import { cn } from "@/lib/utils";
import {
  DEFAULT_LOG_TABLE_FIELDS,
  collectFields,
  getLogsFieldStats,
  safeJsonStringify,
  type LogsTableViewSettings,
} from "./logs-explorer-model";
import {
  EmptyLogs,
  IconAction,
  PaginationControls,
} from "./logs-explorer-shared";
import {
  getDisplayValue,
  useCopyFeedback,
  type CommonViewProps,
  type LogsTraceActionHandler,
} from "./logs-explorer-utils";
import {
  MAX_COLUMN_WIDTH,
  MIN_COLUMN_WIDTH,
  TABLE_ACTIONS_WIDTH,
  columnWidth,
  nextTableSort,
  sortLogRows,
} from "./logs-table-model";
import { useLogsTableColumns } from "./use-logs-table-columns";

export interface LogsTableViewProps extends CommonViewProps {
  settings: LogsTableViewSettings;
  onSettingsChange: (settings: LogsTableViewSettings) => void;
  onOpenRow: (row: LogRow) => void;
  onViewTrace: LogsTraceActionHandler;
}

export function LogsTableView({
  rows,
  settings,
  onSettingsChange,
  onOpenRow,
  onViewTrace,
  onCopy,
  className,
}: LogsTableViewProps) {
  const { t } = useTranslation();
  const fields = useMemo(
    () => collectFields(rows, settings.visibleFields),
    [rows, settings.visibleFields],
  );
  const fieldStats = useMemo(
    () => getLogsFieldStats(rows, fields),
    [fields, rows],
  );
  const { copiedKey, copy } = useCopyFeedback(onCopy);
  const safePageSize = Math.max(1, settings.pageSize);
  const totalTableWidth = useMemo(
    () =>
      settings.visibleFields.reduce(
        (total, field) => total + columnWidth(field, settings.columnWidths),
        TABLE_ACTIONS_WIDTH,
      ),
    [settings.columnWidths, settings.visibleFields],
  );

  const sortedRows = useMemo(
    () => sortLogRows(rows, settings.sort),
    [rows, settings.sort],
  );

  const pageCount = Math.max(1, Math.ceil(sortedRows.length / safePageSize));
  const effectivePage = Math.min(Math.max(1, settings.page), pageCount);
  const pageStart = (effectivePage - 1) * safePageSize;
  const visibleRows = sortedRows.slice(pageStart, pageStart + safePageSize);

  function patchSettings(patch: Partial<LogsTableViewSettings>) {
    onSettingsChange({ ...settings, ...patch });
  }

  const {
    draggedField,
    dropTarget,
    resetColumnWidth,
    moveColumn,
    handleColumnDragStart,
    handleColumnDragOver,
    handleColumnDrop,
    handleColumnDragEnd,
    handleResizeStart,
    handleResizeMove,
    handleResizeEnd,
    handleResizeKeyDown,
  } = useLogsTableColumns(settings, patchSettings);

  function toggleColumn(field: string, checked: boolean) {
    const visibleFields = checked
      ? settings.visibleFields.includes(field)
        ? settings.visibleFields
        : [...settings.visibleFields, field]
      : settings.visibleFields.filter((candidate) => candidate !== field);
    patchSettings({ visibleFields, page: 1 });
  }

  function toggleSort(field: string) {
    patchSettings({ sort: nextTableSort(settings.sort, field), page: 1 });
  }

  return (
    <section
      className={cn(
        "overflow-hidden rounded-md border border-border/70 bg-background",
        className,
      )}
      aria-label={t("logs.tableResults")}
    >
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-border/60 p-2">
        <span className="text-xs text-muted-foreground">
          {t("logs.matchedLines", { count: rows.length })}
        </span>
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button type="button" variant="outline" size="sm">
              <Columns3 />
              {t("logs.columns")}
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent
            align="end"
            className="max-h-[min(32rem,var(--radix-dropdown-menu-content-available-height))] w-80 overflow-y-auto"
          >
            <DropdownMenuLabel>{t("logs.visibleColumns")}</DropdownMenuLabel>
            {fields.map((field) => {
              const stats = fieldStats.get(field);
              const coverage = Math.round(stats?.coveragePercent ?? 0);
              return (
                <DropdownMenuCheckboxItem
                  key={field}
                  checked={settings.visibleFields.includes(field)}
                  onCheckedChange={(checked) =>
                    toggleColumn(field, checked === true)
                  }
                  onSelect={(event) => event.preventDefault()}
                >
                  <span className="flex min-w-0 flex-1 items-center gap-2">
                    <span
                      className="min-w-0 flex-1 truncate font-mono text-xs"
                      title={field}
                    >
                      {field}
                    </span>
                    <span
                      className="shrink-0 text-[10px] tabular-nums text-muted-foreground"
                      title={t("logs.fieldStatsDescription", {
                        present: stats?.presentRows ?? 0,
                        total: rows.length,
                        unique: stats?.uniqueValues ?? 0,
                        coverage,
                      })}
                    >
                      {t("logs.fieldStatsSummary", {
                        unique: stats?.uniqueValues ?? 0,
                        coverage,
                      })}
                    </span>
                  </span>
                </DropdownMenuCheckboxItem>
              );
            })}
            <DropdownMenuSeparator />
            <DropdownMenuItem
              onSelect={() =>
                patchSettings({
                  visibleFields: [...DEFAULT_LOG_TABLE_FIELDS],
                  columnWidths: {},
                  sort: null,
                  page: 1,
                })
              }
            >
              <Columns3 />
              {t("logs.resetColumns")}
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>

      {rows.length === 0 ? (
        <EmptyLogs />
      ) : (
        <div className="overflow-x-auto">
          <table
            className="table-fixed border-collapse text-left text-xs"
            style={{ width: totalTableWidth, minWidth: "100%" }}
          >
            <caption className="sr-only">{t("logs.tableResults")}</caption>
            <colgroup>
              {settings.visibleFields.map((field) => (
                <col
                  key={field}
                  style={{ width: columnWidth(field, settings.columnWidths) }}
                />
              ))}
              <col style={{ width: TABLE_ACTIONS_WIDTH }} />
            </colgroup>
            <thead className="bg-muted/60 text-muted-foreground">
              <tr className="border-b border-border/70">
                {settings.visibleFields.map((field) => {
                  const sortDirection =
                    settings.sort?.field === field
                      ? settings.sort.direction
                      : null;
                  const width = columnWidth(field, settings.columnWidths);
                  const stats = fieldStats.get(field);
                  const isDropTarget = dropTarget?.field === field;
                  return (
                    <th
                      key={field}
                      scope="col"
                      style={{ width }}
                      onDragOver={(event) => handleColumnDragOver(event, field)}
                      onDrop={(event) => handleColumnDrop(event, field)}
                      aria-sort={
                        sortDirection === "asc"
                          ? "ascending"
                          : sortDirection === "desc"
                            ? "descending"
                            : "none"
                      }
                      className={cn(
                        "relative overflow-hidden p-0 font-medium",
                        draggedField === field && "opacity-50",
                        isDropTarget &&
                          dropTarget.position === "before" &&
                          "border-l-2 border-l-primary",
                        isDropTarget &&
                          dropTarget.position === "after" &&
                          "border-r-2 border-r-primary",
                      )}
                    >
                      <div className="flex min-w-0 items-center">
                        <button
                          type="button"
                          draggable
                          className="ml-1 flex size-7 shrink-0 cursor-grab items-center justify-center rounded-sm text-muted-foreground hover:bg-muted active:cursor-grabbing focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                          title={t("logs.dragColumn", { field })}
                          aria-label={t("logs.dragColumn", { field })}
                          onDragStart={(event) =>
                            handleColumnDragStart(event, field)
                          }
                          onDragEnd={handleColumnDragEnd}
                          onKeyDown={(event) => {
                            if (!event.shiftKey) return;
                            if (
                              event.key !== "ArrowLeft" &&
                              event.key !== "ArrowRight"
                            ) {
                              return;
                            }
                            event.preventDefault();
                            moveColumn(
                              field,
                              event.key === "ArrowLeft" ? -1 : 1,
                            );
                          }}
                        >
                          <GripVertical className="size-3.5" />
                        </button>
                        <button
                          type="button"
                          className="flex min-w-0 flex-1 items-center gap-1 rounded-sm px-1 py-2 text-left focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                          title={t("logs.fieldStatsDescription", {
                            present: stats?.presentRows ?? 0,
                            total: rows.length,
                            unique: stats?.uniqueValues ?? 0,
                            coverage: Math.round(
                              stats?.coveragePercent ?? 0,
                            ),
                          })}
                          aria-label={t("logs.sortColumn", { field })}
                          onClick={() => toggleSort(field)}
                        >
                          <span className="min-w-0 truncate font-mono">
                            {field}
                          </span>
                          {sortDirection === "asc" ? (
                            <ArrowUp className="size-3.5 shrink-0" />
                          ) : sortDirection === "desc" ? (
                            <ArrowDown className="size-3.5 shrink-0" />
                          ) : (
                            <ChevronsUpDown className="size-3.5 shrink-0 opacity-50" />
                          )}
                        </button>
                        <div
                          role="separator"
                          tabIndex={0}
                          aria-orientation="vertical"
                          aria-label={t("logs.resizeColumn", { field })}
                          aria-valuemin={MIN_COLUMN_WIDTH}
                          aria-valuemax={MAX_COLUMN_WIDTH}
                          aria-valuenow={width}
                          className="absolute inset-y-0 right-0 z-10 w-2 cursor-col-resize touch-none focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-primary after:absolute after:inset-y-1 after:left-1/2 after:w-px after:bg-border hover:after:bg-primary"
                          onPointerDown={(event) =>
                            handleResizeStart(event, field)
                          }
                          onPointerMove={handleResizeMove}
                          onPointerUp={handleResizeEnd}
                          onPointerCancel={handleResizeEnd}
                          onKeyDown={(event) =>
                            handleResizeKeyDown(event, field)
                          }
                          onDoubleClick={() => resetColumnWidth(field)}
                        />
                      </div>
                    </th>
                  );
                })}
                <th
                  scope="col"
                  style={{ width: TABLE_ACTIONS_WIDTH }}
                  className="sticky right-0 bg-muted/95 px-3 py-2 text-right font-medium shadow-[-1px_0_0_0_var(--border)]"
                >
                  {t("common.actions")}
                </th>
              </tr>
            </thead>
            <tbody>
              {visibleRows.map((row, index) => {
                const copyKey = `table-row:${row.id}:${pageStart + index}`;
                return (
                  <tr
                    key={`${row.id}-${pageStart + index}`}
                    className="group/table-row border-b border-border/50 last:border-0 hover:bg-muted/40"
                  >
                    {settings.visibleFields.map((field) => {
                      const value = getDisplayValue(row, field);
                      return (
                        <td
                          key={field}
                          style={{
                            width: columnWidth(field, settings.columnWidths),
                          }}
                          className={cn(
                            "overflow-hidden px-3 py-1.5 font-mono align-top",
                            field === "_msg"
                              ? "whitespace-pre-wrap break-words"
                              : "whitespace-nowrap",
                          )}
                          title={value}
                        >
                          <span
                            className={cn(
                              "block min-w-0",
                              field !== "_msg" && "truncate",
                            )}
                          >
                            {value}
                          </span>
                        </td>
                      );
                    })}
                    <td className="sticky right-0 bg-background/95 px-2 py-1 text-right shadow-[-1px_0_0_0_var(--border)] group-hover/table-row:bg-muted/95">
                      <div className="flex justify-end gap-0.5">
                        <IconAction
                          label={
                            copiedKey === copyKey
                              ? t("common.copied")
                              : t("logs.copyRow")
                          }
                          onClick={() =>
                            void copy(safeJsonStringify(row.raw), copyKey)
                          }
                        >
                          {copiedKey === copyKey ? <Check /> : <Copy />}
                        </IconAction>
                        <IconAction
                          label={t("logs.rowDetails")}
                          onClick={() => onOpenRow(row)}
                        >
                          <MoreHorizontal />
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
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}

      <PaginationControls
        total={rows.length}
        page={effectivePage}
        pageSize={safePageSize}
        onPageChange={(page) => patchSettings({ page })}
        onPageSizeChange={(pageSize) =>
          patchSettings({ pageSize, page: 1 })
        }
      />
    </section>
  );
}
