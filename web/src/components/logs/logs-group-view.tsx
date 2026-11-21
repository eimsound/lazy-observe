import {
  ArrowDown,
  ArrowUp,
  ArrowUpDown,
  ChevronDown,
  ChevronRight,
  ListCollapse,
  ListTree,
} from "lucide-react";
import { useId, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import type { LogRow } from "@/lib/api/logs";
import { cn } from "@/lib/utils";
import {
  collectFields,
  getRowField,
  groupValueId,
  stringifyFieldValue,
  type LogsGroupViewSettings,
} from "./logs-explorer-model";
import { EmptyLogs, PaginationControls } from "./logs-explorer-shared";
import type {
  CommonViewProps,
  LogsContextActionHandler,
  LogsFieldActionHandler,
  LogsTraceActionHandler,
} from "./logs-explorer-utils";
import { DisplayFieldsMenu } from "./logs-group-options-menu";
import { ExpandableLogRow } from "./logs-group-row";

export interface LogsGroupViewProps extends CommonViewProps {
  settings: LogsGroupViewSettings;
  onSettingsChange: (settings: LogsGroupViewSettings) => void;
  onIncludeField: LogsFieldActionHandler;
  onExcludeField: LogsFieldActionHandler;
  onViewTrace: LogsTraceActionHandler;
  /** Open a stream-context view owned by the page. */
  onShowContext?: LogsContextActionHandler;
}

const GROUP_NONE_TOKEN = "none";
const GROUP_FIELD_TOKEN_PREFIX = "field:";

interface LogGroup {
  id: string;
  label: string;
  rows: LogRow[];
}

function groupFieldToken(field: string | null): string {
  return field === null
    ? GROUP_NONE_TOKEN
    : `${GROUP_FIELD_TOKEN_PREFIX}${encodeURIComponent(field)}`;
}

function fieldFromGroupToken(token: string): string | null {
  if (token === GROUP_NONE_TOKEN) return null;
  if (!token.startsWith(GROUP_FIELD_TOKEN_PREFIX)) return null;
  try {
    return decodeURIComponent(token.slice(GROUP_FIELD_TOKEN_PREFIX.length));
  } catch {
    return token.slice(GROUP_FIELD_TOKEN_PREFIX.length);
  }
}

export function LogsGroupView({
  rows,
  settings,
  onSettingsChange,
  onIncludeField,
  onExcludeField,
  onViewTrace,
  onShowContext,
  onCopy,
  className,
}: LogsGroupViewProps) {
  const { t } = useTranslation();
  const groupContentIdPrefix = useId();
  const fields = useMemo(
    () =>
      collectFields(rows, [
        ...settings.displayFields,
        ...(settings.groupBy === null ? [] : [settings.groupBy]),
      ]),
    [rows, settings.displayFields, settings.groupBy],
  );
  const [expandedGroups, setExpandedGroups] = useState<Set<string>>(
    () => new Set(),
  );
  const markdownParsing = settings.markdownParsing ?? false;
  const ansiParsing = settings.ansiParsing ?? false;
  const disableHover = settings.disableHover ?? false;

  const groups = useMemo<LogGroup[]>(() => {
    if (settings.groupBy === null) return [];
    const grouped = new Map<string, LogGroup>();

    for (const row of rows) {
      const value = getRowField(row, settings.groupBy);
      const valueId = groupValueId(value);
      const id = `${settings.groupBy}:${valueId}`;
      const existing = grouped.get(id);
      if (existing) {
        existing.rows.push(row);
        continue;
      }
      grouped.set(id, {
        id,
        label: value.present
          ? stringifyFieldValue(value.value)
          : t("logs.fieldMissing"),
        rows: [row],
      });
    }

    const direction = settings.groupSort === "asc" ? 1 : -1;
    return [...grouped.values()].sort(
      (left, right) =>
        direction * (left.rows.length - right.rows.length) ||
        left.label.localeCompare(right.label),
    );
  }, [rows, settings.groupBy, settings.groupSort, t]);

  const isGrouped = settings.groupBy !== null;
  const itemCount = isGrouped ? groups.length : rows.length;
  const safePageSize = Math.max(1, settings.pageSize);
  const pageCount = Math.max(1, Math.ceil(itemCount / safePageSize));
  const effectivePage = Math.min(Math.max(1, settings.page), pageCount);
  const pageStart = (effectivePage - 1) * safePageSize;
  const visibleGroups = isGrouped
    ? groups.slice(pageStart, pageStart + safePageSize)
    : [];
  const visibleRows = isGrouped
    ? []
    : rows.slice(pageStart, pageStart + safePageSize);

  function patchSettings(patch: Partial<LogsGroupViewSettings>) {
    onSettingsChange({ ...settings, ...patch });
  }

  function setGroupBy(field: string | null) {
    setExpandedGroups(new Set());
    patchSettings({ groupBy: field, page: 1 });
  }

  function toggleGroup(groupId: string) {
    setExpandedGroups((current) => {
      const next = new Set(current);
      if (next.has(groupId)) next.delete(groupId);
      else next.add(groupId);
      return next;
    });
  }

  function addDisplayField(field: string) {
    if (settings.displayFields.includes(field)) return;
    patchSettings({
      displayFields: [...settings.displayFields, field],
    });
  }

  const rowProps = {
    displayFields: settings.displayFields,
    singleLine: settings.singleLine,
    compact: settings.compactHeader,
    markdownParsing,
    ansiParsing,
    disableHover,
    onIncludeField,
    onExcludeField,
    onSetGroupBy: setGroupBy,
    onAddDisplayField: addDisplayField,
    onViewTrace,
    onShowContext,
    onCopy,
  };

  return (
    <section
      className={cn(
        "overflow-hidden rounded-md border border-border/70 bg-background",
        className,
      )}
      aria-label={t("logs.groupedResults")}
    >
      <div className="flex flex-wrap items-center gap-2 border-b border-border/60 p-2">
        <div className="flex min-w-0 flex-1 flex-wrap items-center gap-2">
          <span className="text-xs font-medium text-muted-foreground">
            {t("logs.groupBy")}
          </span>
          <Select
            value={groupFieldToken(settings.groupBy)}
            onValueChange={(token) => setGroupBy(fieldFromGroupToken(token))}
          >
            <SelectTrigger
              className="h-8 min-w-44 max-w-full sm:w-64"
              aria-label={t("logs.groupBy")}
            >
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={GROUP_NONE_TOKEN}>
                {t("logs.noGrouping")}
              </SelectItem>
              {fields.map((field) => (
                <SelectItem key={field} value={groupFieldToken(field)}>
                  <span className="font-mono text-xs">{field || '""'}</span>
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          {isGrouped && (
            <Button
              type="button"
              variant="outline"
              size="sm"
              title={t(
                settings.groupSort === "desc"
                  ? "logs.sortGroupsAscending"
                  : "logs.sortGroupsDescending",
              )}
              onClick={() =>
                patchSettings({
                  groupSort: settings.groupSort === "desc" ? "asc" : "desc",
                  page: 1,
                })
              }
            >
              <ArrowUpDown />
              {t("logs.groupCount")}
              {settings.groupSort === "desc" ? <ArrowDown /> : <ArrowUp />}
            </Button>
          )}
        </div>

        {isGrouped && (
          <div className="flex items-center gap-1">
            <Button
              type="button"
              variant="ghost"
              size="sm"
              disabled={groups.length === 0}
              onClick={() =>
                setExpandedGroups(
                  new Set([
                    ...expandedGroups,
                    ...groups.map((group) => group.id),
                  ]),
                )
              }
            >
              <ListTree />
              <span className="hidden sm:inline">{t("logs.expandAll")}</span>
            </Button>
            <Button
              type="button"
              variant="ghost"
              size="sm"
              disabled={expandedGroups.size === 0}
              onClick={() => setExpandedGroups(new Set())}
            >
              <ListCollapse />
              <span className="hidden sm:inline">{t("logs.collapseAll")}</span>
            </Button>
          </div>
        )}

        <DisplayFieldsMenu
          fields={fields}
          displayFields={settings.displayFields}
          singleLine={settings.singleLine}
          compactHeader={settings.compactHeader}
          markdownParsing={markdownParsing}
          ansiParsing={ansiParsing}
          disableHover={disableHover}
          onDisplayFieldsChange={(displayFields) =>
            patchSettings({ displayFields, page: 1 })
          }
          onSingleLineChange={(singleLine) => patchSettings({ singleLine })}
          onCompactHeaderChange={(compactHeader) =>
            patchSettings({ compactHeader })
          }
          onMarkdownParsingChange={(enabled) =>
            patchSettings({
              markdownParsing: enabled,
              ansiParsing: enabled ? false : ansiParsing,
            })
          }
          onAnsiParsingChange={(enabled) =>
            patchSettings({
              ansiParsing: enabled,
              markdownParsing: enabled ? false : markdownParsing,
            })
          }
          onDisableHoverChange={(enabled) =>
            patchSettings({ disableHover: enabled })
          }
        />
      </div>

      {rows.length === 0 ? (
        <EmptyLogs />
      ) : isGrouped ? (
        <div className="divide-y divide-border/70">
          {visibleGroups.map((group, groupIndex) => {
            const expanded = expandedGroups.has(group.id);
            const contentId =
              `${groupContentIdPrefix}-group-${pageStart + groupIndex}`;
            return (
              <article key={group.id}>
                <button
                  type="button"
                  className={cn(
                    "flex w-full items-center gap-2 text-left focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring",
                    !disableHover && "hover:bg-muted/50",
                    settings.compactHeader ? "px-2 py-1.5" : "px-3 py-2.5",
                  )}
                  aria-expanded={expanded}
                  aria-controls={contentId}
                  onClick={() => toggleGroup(group.id)}
                >
                  {expanded ? (
                    <ChevronDown className="size-4 shrink-0 text-muted-foreground" />
                  ) : (
                    <ChevronRight className="size-4 shrink-0 text-muted-foreground" />
                  )}
                  <span className="min-w-0 flex-1 truncate font-mono text-xs">
                    <span className="text-muted-foreground">
                      {settings.groupBy}=
                    </span>
                    <span title={group.label}>{group.label}</span>
                  </span>
                  <span className="shrink-0 rounded-full bg-muted px-2 py-0.5 text-xs tabular-nums text-muted-foreground">
                    {t("logs.matchedLines", { count: group.rows.length })}
                  </span>
                </button>
                {expanded && (
                  <div
                    id={contentId}
                    className="divide-y divide-border/50 border-t border-border/60 bg-muted/10"
                  >
                    {group.rows.map((row, index) => (
                      <ExpandableLogRow
                        key={`${row.id}-${index}`}
                        row={row}
                        {...rowProps}
                      />
                    ))}
                  </div>
                )}
              </article>
            );
          })}
        </div>
      ) : (
        <div className="divide-y divide-border/50">
          {visibleRows.map((row, index) => (
            <ExpandableLogRow
              key={`${row.id}-${pageStart + index}`}
              row={row}
              {...rowProps}
            />
          ))}
        </div>
      )}

      <PaginationControls
        total={itemCount}
        page={effectivePage}
        pageSize={safePageSize}
        pageSizeLabel={
          isGrouped ? t("logs.groupsPerPage") : t("logs.rowsPerPage")
        }
        onPageChange={(page) => patchSettings({ page })}
        onPageSizeChange={(pageSize) =>
          patchSettings({ pageSize, page: 1 })
        }
      />
    </section>
  );
}
