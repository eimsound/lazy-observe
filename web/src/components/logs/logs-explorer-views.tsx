import {
  ArrowDown,
  ArrowUp,
  ArrowUpDown,
  Braces,
  Check,
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  ChevronUp,
  ChevronsUpDown,
  Columns3,
  Copy,
  Eye,
  FileText,
  Filter,
  FilterX,
  GripVertical,
  ListCollapse,
  ListTree,
  MoreHorizontal,
  MousePointerBan,
  PanelTopOpen,
  Palette,
  Rows3,
  Search,
  Settings2,
  Waypoints,
  WrapText,
} from "lucide-react";
import {
  useCallback,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
  type DragEvent,
  type KeyboardEvent,
  type PointerEvent,
  type ReactNode,
} from "react";
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
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import type { LogRow } from "@/lib/api/logs";
import { formatDateTime } from "@/lib/format";
import { cn } from "@/lib/utils";
import { LevelBadge } from "./level-indicator";
import {
  DEFAULT_LOG_TABLE_FIELDS,
  safeJsonStringify,
  stringifyFieldValue,
  getRowField,
  collectFields,
  groupValueId,
  getLogsFieldStats,
  type LogsGroupViewSettings,
  type LogsTableViewSettings,
} from "./logs-explorer-model";
export type {
  LogsSortDirection,
  LogsGroupViewSettings,
  LogsTableSort,
  LogsTableViewSettings,
  LogsFieldStats,
} from "./logs-explorer-model";
import { renderAnsiMessage } from "./log-message-renderers";
import { SafeMarkdownMessage } from "./safe-markdown-message";

export type LogsCopyHandler = (text: string) => void | Promise<void>;
export type LogsFieldActionHandler = (
  field: string,
  value: string,
  row: LogRow,
) => void;
export type LogsTraceActionHandler = (traceId: string, row: LogRow) => void;
export type LogsContextActionHandler = (row: LogRow) => void;




interface CommonViewProps {
  rows: LogRow[];
  className?: string;
  /** Override clipboard handling, for example to add a toast or test the view. */
  onCopy?: LogsCopyHandler;
}

export interface LogsGroupViewProps extends CommonViewProps {
  settings: LogsGroupViewSettings;
  onSettingsChange: (settings: LogsGroupViewSettings) => void;
  onIncludeField: LogsFieldActionHandler;
  onExcludeField: LogsFieldActionHandler;
  onViewTrace: LogsTraceActionHandler;
  /** Open a stream-context view owned by the page. */
  onShowContext?: LogsContextActionHandler;
}

export interface LogsTableViewProps extends CommonViewProps {
  settings: LogsTableViewSettings;
  onSettingsChange: (settings: LogsTableViewSettings) => void;
  onOpenRow: (row: LogRow) => void;
  onViewTrace: LogsTraceActionHandler;
}

export type LogsJsonViewProps = CommonViewProps;





const PAGE_SIZE_OPTIONS = [10, 25, 50, 100];
const GROUP_NONE_TOKEN = "none";
const GROUP_FIELD_TOKEN_PREFIX = "field:";
const MIN_COLUMN_WIDTH = 96;
const MAX_COLUMN_WIDTH = 800;
const TABLE_ACTIONS_WIDTH = 116;


interface LogGroup {
  id: string;
  label: string;
  rows: LogRow[];
}

interface ColumnDropTarget {
  field: string;
  position: "before" | "after";
}

interface ColumnResizeState {
  field: string;
  pointerId: number;
  startX: number;
  startWidth: number;
}



function fieldActionValue(value: unknown): string {
  if (value === null) return "null";
  if (value === undefined) return "";
  if (typeof value === "string") return value;
  if (typeof value === "object") return safeJsonStringify(value);
  return String(value);
}


function getDisplayValue(row: LogRow, field: string): string {
  if (field === "_time" && row.time > 0) return formatDateTime(row.time);
  return stringifyFieldValue(getRowField(row, field).value);
}



function defaultColumnWidth(field: string): number {
  if (field === "_time") return 200;
  if (field === "_msg") return 420;
  if (field === "_stream") return 260;
  return 190;
}

function clampColumnWidth(width: number): number {
  return Math.min(MAX_COLUMN_WIDTH, Math.max(MIN_COLUMN_WIDTH, Math.round(width)));
}

function columnWidth(
  field: string,
  widths: Record<string, number> | undefined,
): number {
  const configured = widths?.[field];
  return typeof configured === "number" && Number.isFinite(configured)
    ? clampColumnWidth(configured)
    : defaultColumnWidth(field);
}

function reorderColumns(
  fields: ReadonlyArray<string>,
  source: string,
  target: string,
  position: ColumnDropTarget["position"],
): string[] {
  if (source === target || !fields.includes(source) || !fields.includes(target)) {
    return [...fields];
  }
  const next = fields.filter((field) => field !== source);
  const targetIndex = next.indexOf(target);
  const insertIndex = targetIndex + (position === "after" ? 1 : 0);
  next.splice(insertIndex, 0, source);
  return next;
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


async function writeClipboard(text: string): Promise<void> {
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

function useCopyFeedback(onCopy?: LogsCopyHandler) {
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

function IconAction({
  label,
  children,
  disabled,
  onClick,
  className,
}: {
  label: string;
  children: ReactNode;
  disabled?: boolean;
  onClick: () => void;
  className?: string;
}) {
  return (
    <Button
      type="button"
      variant="ghost"
      size="icon"
      className={cn("size-7 shrink-0", className)}
      title={label}
      aria-label={label}
      disabled={disabled}
      onClick={onClick}
    >
      {children}
    </Button>
  );
}

function PaginationControls({
  total,
  page,
  pageSize,
  pageSizeLabel,
  onPageChange,
  onPageSizeChange,
}: {
  total: number;
  page: number;
  pageSize: number;
  pageSizeLabel?: string;
  onPageChange: (page: number) => void;
  onPageSizeChange: (pageSize: number) => void;
}) {
  const { t } = useTranslation();
  const safePageSize = Math.max(1, pageSize);
  const pageCount = Math.max(1, Math.ceil(total / safePageSize));
  const effectivePage = Math.min(Math.max(1, page), pageCount);
  const from = total === 0 ? 0 : (effectivePage - 1) * safePageSize + 1;
  const to = Math.min(effectivePage * safePageSize, total);
  const pageSizes = PAGE_SIZE_OPTIONS.includes(safePageSize)
    ? PAGE_SIZE_OPTIONS
    : [...PAGE_SIZE_OPTIONS, safePageSize].sort((left, right) => left - right);

  return (
    <div className="flex flex-wrap items-center justify-between gap-2 border-t border-border/60 px-2 py-2 text-xs text-muted-foreground">
      <span aria-live="polite">
        {t("logs.paginationStatus", { from, to, total })}
      </span>
      <div className="flex items-center gap-2">
        <span className="hidden sm:inline">
          {pageSizeLabel ?? t("logs.rowsPerPage")}
        </span>
        <Select
          value={String(safePageSize)}
          onValueChange={(value) => onPageSizeChange(Number(value))}
        >
          <SelectTrigger
            className="h-8 w-[4.75rem]"
            aria-label={pageSizeLabel ?? t("logs.rowsPerPage")}
          >
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {pageSizes.map((option) => (
              <SelectItem key={option} value={String(option)}>
                {option}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Button
          type="button"
          variant="outline"
          size="icon"
          className="size-8"
          aria-label={t("logs.previousPage")}
          title={t("logs.previousPage")}
          disabled={effectivePage <= 1}
          onClick={() => onPageChange(effectivePage - 1)}
        >
          <ChevronLeft />
        </Button>
        <span className="min-w-14 text-center tabular-nums text-foreground">
          {effectivePage} / {pageCount}
        </span>
        <Button
          type="button"
          variant="outline"
          size="icon"
          className="size-8"
          aria-label={t("logs.nextPage")}
          title={t("logs.nextPage")}
          disabled={effectivePage >= pageCount}
          onClick={() => onPageChange(effectivePage + 1)}
        >
          <ChevronRight />
        </Button>
      </div>
    </div>
  );
}

function DisplayFieldsMenu({
  fields,
  displayFields,
  singleLine,
  compactHeader,
  markdownParsing,
  ansiParsing,
  disableHover,
  onDisplayFieldsChange,
  onSingleLineChange,
  onCompactHeaderChange,
  onMarkdownParsingChange,
  onAnsiParsingChange,
  onDisableHoverChange,
}: {
  fields: string[];
  displayFields: string[];
  singleLine: boolean;
  compactHeader: boolean;
  markdownParsing: boolean;
  ansiParsing: boolean;
  disableHover: boolean;
  onDisplayFieldsChange: (fields: string[]) => void;
  onSingleLineChange: (value: boolean) => void;
  onCompactHeaderChange: (value: boolean) => void;
  onMarkdownParsingChange: (value: boolean) => void;
  onAnsiParsingChange: (value: boolean) => void;
  onDisableHoverChange: (value: boolean) => void;
}) {
  const { t } = useTranslation();

  function toggleField(field: string, checked: boolean) {
    if (checked) {
      if (!displayFields.includes(field)) {
        onDisplayFieldsChange([...displayFields, field]);
      }
      return;
    }
    onDisplayFieldsChange(
      displayFields.filter((candidate) => candidate !== field),
    );
  }

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button type="button" variant="outline" size="sm">
          <Settings2 />
          {t("logs.viewOptions")}
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent
        align="end"
        className="max-h-[min(32rem,var(--radix-dropdown-menu-content-available-height))] w-64 overflow-y-auto"
      >
        <DropdownMenuLabel>{t("logs.displayFields")}</DropdownMenuLabel>
        {fields.map((field) => (
          <DropdownMenuCheckboxItem
            key={field}
            checked={displayFields.includes(field)}
            onCheckedChange={(checked) => toggleField(field, checked === true)}
            onSelect={(event) => event.preventDefault()}
          >
            <span className="truncate font-mono text-xs" title={field}>
              {field}
            </span>
          </DropdownMenuCheckboxItem>
        ))}
        <DropdownMenuSeparator />
        <DropdownMenuCheckboxItem
          checked={singleLine}
          onCheckedChange={(checked) => onSingleLineChange(checked === true)}
          onSelect={(event) => event.preventDefault()}
        >
          <WrapText className="mr-2 size-4" />
          {t("logs.singleLine")}
        </DropdownMenuCheckboxItem>
        <DropdownMenuCheckboxItem
          checked={compactHeader}
          onCheckedChange={(checked) => onCompactHeaderChange(checked === true)}
          onSelect={(event) => event.preventDefault()}
        >
          <Rows3 className="mr-2 size-4" />
          {t("logs.compactGroupHeader")}
        </DropdownMenuCheckboxItem>
        <DropdownMenuSeparator />
        <DropdownMenuLabel>{t("logs.messageRendering")}</DropdownMenuLabel>
        <DropdownMenuCheckboxItem
          checked={markdownParsing}
          onCheckedChange={(checked) =>
            onMarkdownParsingChange(checked === true)
          }
          onSelect={(event) => event.preventDefault()}
        >
          <FileText className="mr-2 size-4" />
          {t("logs.markdownParsing")}
        </DropdownMenuCheckboxItem>
        <DropdownMenuCheckboxItem
          checked={ansiParsing}
          onCheckedChange={(checked) => onAnsiParsingChange(checked === true)}
          onSelect={(event) => event.preventDefault()}
        >
          <Palette className="mr-2 size-4" />
          {t("logs.ansiParsing")}
        </DropdownMenuCheckboxItem>
        <DropdownMenuSeparator />
        <DropdownMenuCheckboxItem
          checked={disableHover}
          onCheckedChange={(checked) => onDisableHoverChange(checked === true)}
          onSelect={(event) => event.preventDefault()}
        >
          <MousePointerBan className="mr-2 size-4" />
          {t("logs.disableHoverEffects")}
        </DropdownMenuCheckboxItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

function EmptyLogs() {
  const { t } = useTranslation();
  return (
    <div className="flex min-h-40 items-center justify-center px-4 text-center text-sm text-muted-foreground">
      {t("logs.emptyTitle")}
    </div>
  );
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

interface ExpandableLogRowProps {
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

function ExpandableLogRow({
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
  const [draggedField, setDraggedField] = useState<string | null>(null);
  const [dropTarget, setDropTarget] = useState<ColumnDropTarget | null>(null);
  const resizeRef = useRef<ColumnResizeState | null>(null);
  const totalTableWidth = useMemo(
    () =>
      settings.visibleFields.reduce(
        (total, field) => total + columnWidth(field, settings.columnWidths),
        TABLE_ACTIONS_WIDTH,
      ),
    [settings.columnWidths, settings.visibleFields],
  );

  const sortedRows = useMemo(() => {
    if (!settings.sort) return rows;
    const { field, direction } = settings.sort;
    const multiplier = direction === "asc" ? 1 : -1;

    return rows
      .map((row, index) => ({ row, index }))
      .sort((left, right) => {
        const comparison = compareRows(left.row, right.row, field);
        return comparison === 0
          ? left.index - right.index
          : comparison * multiplier;
      })
      .map(({ row }) => row);
  }, [rows, settings.sort]);

  const pageCount = Math.max(1, Math.ceil(sortedRows.length / safePageSize));
  const effectivePage = Math.min(Math.max(1, settings.page), pageCount);
  const pageStart = (effectivePage - 1) * safePageSize;
  const visibleRows = sortedRows.slice(pageStart, pageStart + safePageSize);

  function patchSettings(patch: Partial<LogsTableViewSettings>) {
    onSettingsChange({ ...settings, ...patch });
  }

  function toggleColumn(field: string, checked: boolean) {
    const visibleFields = checked
      ? settings.visibleFields.includes(field)
        ? settings.visibleFields
        : [...settings.visibleFields, field]
      : settings.visibleFields.filter((candidate) => candidate !== field);
    patchSettings({ visibleFields, page: 1 });
  }

  function toggleSort(field: string) {
    const current = settings.sort;
    if (!current || current.field !== field) {
      patchSettings({ sort: { field, direction: "asc" }, page: 1 });
      return;
    }
    if (current.direction === "asc") {
      patchSettings({ sort: { field, direction: "desc" }, page: 1 });
      return;
    }
    patchSettings({ sort: null, page: 1 });
  }

  function setColumnWidth(field: string, width: number) {
    patchSettings({
      columnWidths: {
        ...(settings.columnWidths ?? {}),
        [field]: clampColumnWidth(width),
      },
    });
  }

  function resetColumnWidth(field: string) {
    const columnWidths = { ...(settings.columnWidths ?? {}) };
    delete columnWidths[field];
    patchSettings({ columnWidths });
  }

  function moveColumn(field: string, offset: -1 | 1) {
    const from = settings.visibleFields.indexOf(field);
    const to = from + offset;
    if (from < 0 || to < 0 || to >= settings.visibleFields.length) return;
    const visibleFields = [...settings.visibleFields];
    [visibleFields[from], visibleFields[to]] = [
      visibleFields[to],
      visibleFields[from],
    ];
    patchSettings({ visibleFields });
  }

  function handleColumnDragStart(
    event: DragEvent<HTMLButtonElement>,
    field: string,
  ) {
    event.stopPropagation();
    event.dataTransfer.effectAllowed = "move";
    event.dataTransfer.setData("text/plain", field);
    setDraggedField(field);
    setDropTarget(null);
  }

  function handleColumnDragOver(
    event: DragEvent<HTMLTableCellElement>,
    field: string,
  ) {
    if (!draggedField || draggedField === field) return;
    event.preventDefault();
    event.dataTransfer.dropEffect = "move";
    const bounds = event.currentTarget.getBoundingClientRect();
    const position =
      event.clientX < bounds.left + bounds.width / 2 ? "before" : "after";
    setDropTarget((current) =>
      current?.field === field && current.position === position
        ? current
        : { field, position },
    );
  }

  function handleColumnDrop(
    event: DragEvent<HTMLTableCellElement>,
    field: string,
  ) {
    event.preventDefault();
    const source = draggedField || event.dataTransfer.getData("text/plain");
    const position =
      dropTarget?.field === field ? dropTarget.position : "before";
    if (source) {
      patchSettings({
        visibleFields: reorderColumns(
          settings.visibleFields,
          source,
          field,
          position,
        ),
      });
    }
    setDraggedField(null);
    setDropTarget(null);
  }

  function handleColumnDragEnd() {
    setDraggedField(null);
    setDropTarget(null);
  }

  function handleResizeStart(
    event: PointerEvent<HTMLDivElement>,
    field: string,
  ) {
    event.preventDefault();
    event.stopPropagation();
    resizeRef.current = {
      field,
      pointerId: event.pointerId,
      startX: event.clientX,
      startWidth: columnWidth(field, settings.columnWidths),
    };
    event.currentTarget.setPointerCapture(event.pointerId);
  }

  function handleResizeMove(event: PointerEvent<HTMLDivElement>) {
    const resize = resizeRef.current;
    if (!resize || resize.pointerId !== event.pointerId) return;
    event.preventDefault();
    setColumnWidth(
      resize.field,
      resize.startWidth + event.clientX - resize.startX,
    );
  }

  function handleResizeEnd(event: PointerEvent<HTMLDivElement>) {
    const resize = resizeRef.current;
    if (!resize || resize.pointerId !== event.pointerId) return;
    resizeRef.current = null;
    if (event.currentTarget.hasPointerCapture(event.pointerId)) {
      event.currentTarget.releasePointerCapture(event.pointerId);
    }
  }

  function handleResizeKeyDown(
    event: KeyboardEvent<HTMLDivElement>,
    field: string,
  ) {
    if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") return;
    event.preventDefault();
    event.stopPropagation();
    const delta = (event.shiftKey ? 50 : 10) *
      (event.key === "ArrowRight" ? 1 : -1);
    setColumnWidth(
      field,
      columnWidth(field, settings.columnWidths) + delta,
    );
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

function compareRows(left: LogRow, right: LogRow, field: string): number {
  if (field === "_time") return left.time - right.time;

  const leftField = getRowField(left, field);
  const rightField = getRowField(right, field);
  if (!leftField.present && !rightField.present) return 0;
  if (!leftField.present) return 1;
  if (!rightField.present) return -1;

  if (
    typeof leftField.value === "number" &&
    typeof rightField.value === "number"
  ) {
    return leftField.value - rightField.value;
  }

  return stringifyFieldValue(leftField.value).localeCompare(
    stringifyFieldValue(rightField.value),
    undefined,
    { numeric: true, sensitivity: "base" },
  );
}

export function LogsJsonView({
  rows,
  onCopy,
  className,
}: LogsJsonViewProps) {
  const { t } = useTranslation();
  const { copiedKey, copy } = useCopyFeedback(onCopy);
  const jsonLines = useMemo(
    () => rows.map((row) => safeJsonStringify(row.raw)).join("\n"),
    [rows],
  );

  return (
    <section
      className={cn(
        "overflow-hidden rounded-md border border-border/70 bg-background",
        className,
      )}
      aria-label={t("logs.jsonLines")}
    >
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-border/60 p-2">
        <span className="inline-flex items-center gap-2 text-xs text-muted-foreground">
          <Braces className="size-4" />
          {t("logs.jsonLines")}
          <span className="tabular-nums">
            {t("logs.matchedLines", { count: rows.length })}
          </span>
        </span>
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={rows.length === 0}
          onClick={() => void copy(jsonLines, "jsonl")}
        >
          {copiedKey === "jsonl" ? <Check /> : <Copy />}
          {copiedKey === "jsonl"
            ? t("common.copied")
            : t("logs.copyAllJson")}
        </Button>
      </div>

      {rows.length === 0 ? (
        <EmptyLogs />
      ) : (
        <pre
          className="max-h-[640px] overflow-auto whitespace-pre p-3 font-mono text-xs leading-relaxed text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring"
          tabIndex={0}
        >
          {jsonLines}
        </pre>
      )}
    </section>
  );
}
