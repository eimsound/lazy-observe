import {
  useEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
  type KeyboardEvent as ReactKeyboardEvent,
  type PointerEvent as ReactPointerEvent,
} from "react";
import { createPortal } from "react-dom";
import { useTranslation } from "react-i18next";
import {
  ArrowDownUp,
  Check,
  ChevronDown,
  ChevronRight,
  ChevronsDownUp,
  ChevronsUpDown,
  Copy,
  LoaderCircle,
  Minus,
  RotateCcw,
  Search,
  X,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import type { LogsFieldValue } from "@/lib/api/logs";
import {
  buildLogsExtraFilterParams,
  logsFilterLabel,
  type LogsExtraFilter,
} from "@/lib/logs-explorer";
import { cn } from "@/lib/utils";

type SortDirection = "asc" | "desc";

const SIDEBAR_WIDTH_STORAGE_KEY = "LOGS_FILTER_SIDEBAR_WIDTH";
const DEFAULT_SIDEBAR_WIDTH = 320;
const MIN_SIDEBAR_WIDTH = 250;
const SIDEBAR_KEYBOARD_STEP = 16;

function parseStoredSidebarWidth(raw: string | null): number | undefined {
  if (!raw) return undefined;
  const width = Number(raw);
  return Number.isFinite(width) && width >= MIN_SIDEBAR_WIDTH ? width : undefined;
}

function readStoredSidebarWidth(): number | undefined {
  if (typeof window === "undefined") return undefined;
  try {
    return parseStoredSidebarWidth(window.localStorage.getItem(SIDEBAR_WIDTH_STORAGE_KEY));
  } catch {
    return undefined;
  }
}

function persistSidebarWidth(width: number | undefined) {
  try {
    if (width === undefined) window.localStorage.removeItem(SIDEBAR_WIDTH_STORAGE_KEY);
    else window.localStorage.setItem(SIDEBAR_WIDTH_STORAGE_KEY, String(Math.round(width)));
  } catch {
    // Storage is optional in privacy-restricted browsers.
  }
}

function clampSidebarWidth(width: number): number {
  const availableWidth = typeof window === "undefined"
    ? width
    : Math.max(MIN_SIDEBAR_WIDTH, window.innerWidth - MIN_SIDEBAR_WIDTH);
  return Math.min(Math.max(width, MIN_SIDEBAR_WIDTH), availableWidth);
}

export interface LogsFilterSidebarProps {
  /** Controls both the desktop sidebar and the responsive mobile drawer. */
  open?: boolean;
  fields: LogsFieldValue[];
  fieldsLoading?: boolean;
  fieldsError?: string | null;
  /** Values are keyed by their field name. An absent key means they have not loaded yet. */
  fieldValues: Readonly<Record<string, LogsFieldValue[] | undefined>>;
  fieldValuesLoading?: Readonly<Record<string, boolean | undefined>>;
  fieldValuesErrors?: Readonly<Record<string, string | null | undefined>>;
  filters: LogsExtraFilter[];
  onRequestFieldValues: (field: string) => void;
  onUpsertFilter: (filter: LogsExtraFilter) => void;
  onRemoveFilter: (field: string, value: string) => void;
  onRemoveField: (field: string) => void;
  onClearFilters: () => void;
  onClose: () => void;
}

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

function appendSelectedValues(
  values: readonly LogsFieldValue[],
  filters: readonly LogsExtraFilter[],
): LogsFieldValue[] {
  const result = [...values];
  const knownValues = new Set(values.map((item) => item.value));
  for (const filter of filters) {
    if (filter.value === "" || knownValues.has(filter.value)) continue;
    result.push({ value: filter.value, hits: 0 });
    knownValues.add(filter.value);
  }
  return result;
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

interface FilterValueRowProps {
  field: string;
  item: LogsFieldValue;
  filter?: LogsExtraFilter;
  implicitlySelected?: boolean;
  numberFormatter: Intl.NumberFormat;
  onUpsertFilter: (filter: LogsExtraFilter) => void;
  onRemoveFilter: (field: string, value: string) => void;
}

function FilterValueRow({
  field,
  item,
  filter,
  implicitlySelected = false,
  numberFormatter,
  onUpsertFilter,
  onRemoveFilter,
}: FilterValueRowProps) {
  const { t } = useTranslation();
  const selected = Boolean(filter);
  const checked = selected || implicitlySelected;
  const excluded = filter?.operator === "!=" || filter?.operator === "!~";

  const upsert = (operator: "=" | "!=") => {
    onUpsertFilter({
      field,
      value: item.value,
      operator,
      isStream: true,
    });
  };

  return (
    <div
      className={cn(
        "group flex min-w-0 items-center gap-1 rounded-md border border-transparent pl-2 transition-colors",
        selected ? "bg-primary/10 text-foreground" : "text-muted-foreground hover:bg-accent/70",
        item.hits === 0 && "opacity-70",
      )}
    >
      <button
        type="button"
        className="flex min-w-0 flex-1 items-center gap-2 py-1.5 text-left text-xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
        aria-pressed={checked}
        onClick={() => {
          if (selected) onRemoveFilter(field, item.value);
          else upsert("=");
        }}
      >
        <span
          className={cn(
            "flex size-4 shrink-0 items-center justify-center rounded border",
            selected
              ? "border-primary bg-primary text-primary-foreground"
              : "border-input bg-background",
          )}
          aria-hidden="true"
        >
          {checked && (
            selected && excluded ? <Minus className="size-3" /> : <Check className="size-3" />
          )}
        </span>
        <span className="min-w-0 flex-1 truncate font-mono" title={item.value}>
          {item.value || t("logs.emptyValue")}
        </span>
        <span className="shrink-0 tabular-nums text-muted-foreground">
          {numberFormatter.format(item.hits)}
        </span>
      </button>

      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className={cn(
              "mr-0.5 h-7 min-w-8 px-1.5 font-mono text-xs",
              selected ? "opacity-100" : "opacity-0 group-hover:opacity-100 focus:opacity-100",
            )}
            aria-label={t("logs.selectFilterOperator", { value: item.value })}
          >
            {excluded ? "!=" : "="}
            <ChevronDown className="size-3" aria-hidden="true" />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuLabel>{t("logs.filterValue")}</DropdownMenuLabel>
          <DropdownMenuItem onSelect={() => upsert("=")}>
            <Check
              className={cn("size-4", filter?.operator !== "=" && "invisible")}
              aria-hidden="true"
            />
            <span className="font-mono">=</span>
            {t("logs.includeValue")}
          </DropdownMenuItem>
          <DropdownMenuItem onSelect={() => upsert("!=")}>
            <Check
              className={cn("size-4", filter?.operator !== "!=" && "invisible")}
              aria-hidden="true"
            />
            <span className="font-mono">!=</span>
            {t("logs.excludeValue")}
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    </div>
  );
}

interface FilterFieldProps {
  field: LogsFieldValue;
  expanded: boolean;
  values: LogsFieldValue[] | undefined;
  valuesLoading: boolean;
  valuesError?: string | null;
  filters: LogsExtraFilter[];
  sortDirection: SortDirection;
  numberFormatter: Intl.NumberFormat;
  onToggleExpanded: () => void;
  onUpsertFilter: (filter: LogsExtraFilter) => void;
  onRemoveFilter: (field: string, value: string) => void;
  onRemoveField: (field: string) => void;
}

function FilterField({
  field,
  expanded,
  values,
  valuesLoading,
  valuesError,
  filters,
  sortDirection,
  numberFormatter,
  onToggleExpanded,
  onUpsertFilter,
  onRemoveFilter,
  onRemoveField,
}: FilterFieldProps) {
  const { t } = useTranslation();
  const fieldFilters = filters.filter((filter) => (
    filter.isStream && filter.field === field.value
  ));
  const anyFilter = fieldFilters.find((filter) => (
    filter.value === "" && filter.operator === "!="
  ));
  const allListedValuesSelected = Boolean(
    values?.length
    && values.every((item) => fieldFilters.some((filter) => filter.value === item.value)),
  );
  const badgeLabel = anyFilter
    ? t("logs.anyValue")
    : allListedValuesSelected
      ? t("logs.allValues")
      : fieldFilters.length;
  const badgeTitle = anyFilter
    ? t("logs.anyNonEmptyValue")
    : allListedValuesSelected
      ? t("logs.allListedValues")
      : t("logs.selectedValues", { count: fieldFilters.length });
  const hasFilter = fieldFilters.length > 0;
  const listedValues = useMemo(() => {
    const allValues = appendSelectedValues(values ?? [], fieldFilters);
    return allValues.sort((a, b) => {
      const hitsOrder = sortDirection === "desc" ? b.hits - a.hits : a.hits - b.hits;
      return hitsOrder || a.value.localeCompare(b.value);
    });
  }, [fieldFilters, sortDirection, values]);
  const valuesId = `logs-filter-values-${encodeURIComponent(field.value)}`;

  return (
    <li
      role="treeitem"
      aria-expanded={expanded}
      aria-level={1}
      className={cn(
        "min-w-0",
        field.hits === 0 && "opacity-80",
      )}
    >
      <div
        className={cn(
          "flex min-w-0 items-center gap-1 rounded-md px-1 py-0.5 transition-colors",
          fieldFilters.length ? "bg-primary/[0.08]" : "hover:bg-accent/70",
        )}
      >
        <button
          type="button"
          className="flex size-7 shrink-0 items-center justify-center rounded text-muted-foreground hover:bg-accent hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          aria-label={
            expanded
              ? t("logs.collapseField", { field: field.value })
              : t("logs.expandField", { field: field.value })
          }
          aria-controls={valuesId}
          onClick={onToggleExpanded}
          onKeyDown={(event) => {
            if (event.key === "ArrowRight" && !expanded) {
              event.preventDefault();
              onToggleExpanded();
            }
            if (event.key === "ArrowLeft" && expanded) {
              event.preventDefault();
              onToggleExpanded();
            }
          }}
        >
          <ChevronRight
            className={cn("size-4 transition-transform", expanded && "rotate-90")}
            aria-hidden="true"
          />
        </button>
        <Button
          type="button"
          variant="ghost"
          size="icon"
          className="size-7 shrink-0"
          aria-pressed={hasFilter}
          aria-label={
            hasFilter
              ? t("logs.clearFieldFilters", { field: field.value })
              : t("logs.filterAnyValue", { field: field.value })
          }
          title={hasFilter ? badgeTitle : t("logs.filterAnyValue", { field: field.value })}
          onClick={() => {
            if (hasFilter) {
              onRemoveField(field.value);
              return;
            }
            onUpsertFilter({
              field: field.value,
              value: "",
              operator: "!=",
              isStream: true,
            });
          }}
        >
          <span
            className={cn(
              "flex size-4 items-center justify-center rounded border",
              hasFilter
                ? "border-primary bg-primary text-primary-foreground"
                : "border-input bg-background",
            )}
            aria-hidden="true"
          >
            {hasFilter && (
              anyFilter || allListedValuesSelected
                ? <Check className="size-3" />
                : <Minus className="size-3" />
            )}
          </span>
        </Button>
        <button
          type="button"
          className="flex min-w-0 flex-1 items-center gap-2 rounded px-1.5 py-1.5 text-left text-xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          aria-expanded={expanded}
          aria-controls={valuesId}
          onClick={onToggleExpanded}
        >
          <span className="min-w-0 flex-1 truncate font-mono" title={field.value}>
            {field.value}
          </span>
          <span className="shrink-0 text-xs tabular-nums text-muted-foreground">
            {numberFormatter.format(field.hits)}
          </span>
          {fieldFilters.length > 0 && (
            <span
              className="flex min-w-5 shrink-0 items-center justify-center rounded-full bg-primary px-1.5 py-0.5 text-[10px] font-semibold text-primary-foreground"
              title={badgeTitle}
            >
              {badgeLabel}
            </span>
          )}
        </button>
      </div>

      {expanded && (
        <ul
          id={valuesId}
          role="group"
          className="ml-4 border-l border-border/80 py-1 pl-3"
        >
          {valuesLoading && (
            <li
              role="none"
              className="flex items-center gap-2 px-2 py-3 text-xs text-muted-foreground"
            >
              <span className="flex items-center gap-2" role="status">
                <LoaderCircle className="size-3.5 animate-spin" aria-hidden="true" />
                {t("logs.loadingFieldValues")}
              </span>
            </li>
          )}
          {valuesError && (
            <li role="none">
              <div
                className="rounded bg-destructive/10 px-2 py-2 text-xs text-destructive"
                role="alert"
              >
                <span className="font-medium">{t("logs.fieldValuesLoadError")}</span>
                <span className="mt-0.5 block break-words">{valuesError}</span>
              </div>
            </li>
          )}
          {!valuesLoading && !valuesError && values !== undefined && listedValues.length === 0 && (
            <li role="none">
              <p className="px-2 py-3 text-xs text-muted-foreground">{t("logs.noFieldValues")}</p>
            </li>
          )}
          {listedValues.map((item) => {
            const filter = fieldFilters.find((candidate) => candidate.value === item.value);
            return (
              <li
                key={item.value}
                role="treeitem"
                aria-level={2}
                aria-selected={Boolean(filter || anyFilter)}
                className="relative before:absolute before:-left-3 before:top-1/2 before:w-3 before:border-t before:border-border/80"
              >
                <FilterValueRow
                  field={field.value}
                  item={item}
                  filter={filter}
                  implicitlySelected={Boolean(anyFilter)}
                  numberFormatter={numberFormatter}
                  onUpsertFilter={onUpsertFilter}
                  onRemoveFilter={onRemoveFilter}
                />
              </li>
            );
          })}
        </ul>
      )}
    </li>
  );
}

export function LogsFilterSidebar({
  open = true,
  fields,
  fieldsLoading = false,
  fieldsError,
  fieldValues,
  fieldValuesLoading = {},
  fieldValuesErrors = {},
  filters,
  onRequestFieldValues,
  onUpsertFilter,
  onRemoveFilter,
  onRemoveField,
  onClearFilters,
  onClose,
}: LogsFilterSidebarProps) {
  const { t, i18n } = useTranslation();
  const [search, setSearch] = useState("");
  const [sortDirection, setSortDirection] = useState<SortDirection>("desc");
  const [expandedFields, setExpandedFields] = useState<Set<string>>(() => new Set());
  const [sidebarWidth, setSidebarWidth] = useState<number | undefined>(readStoredSidebarWidth);
  const [isResizing, setIsResizing] = useState(false);
  const sidebarRef = useRef<HTMLElement>(null);
  const currentWidth = useRef(sidebarWidth ?? DEFAULT_SIDEBAR_WIDTH);
  const resizeStart = useRef({ pointerX: 0, width: DEFAULT_SIDEBAR_WIDTH });
  const requestedFields = useRef(new Set<string>());
  const numberFormatter = useMemo(
    () => new Intl.NumberFormat(i18n.language, { notation: "compact", maximumFractionDigits: 1 }),
    [i18n.language],
  );
  const sidebarStyle = {
    "--logs-filter-sidebar-width": `${sidebarWidth ?? DEFAULT_SIDEBAR_WIDTH}px`,
  } as CSSProperties;

  const allFields = useMemo(() => {
    const allFields = [...fields];
    const knownFields = new Set(fields.map((field) => field.value));
    for (const filter of filters) {
      if (!filter.isStream || knownFields.has(filter.field)) continue;
      allFields.push({ value: filter.field, hits: 0 });
      knownFields.add(filter.field);
    }
    return allFields;
  }, [fields, filters]);

  const visibleFields = useMemo(() => {
    const normalizedSearch = search.trim().toLocaleLowerCase(i18n.language);
    return allFields
      .filter((field) => (
        !normalizedSearch
        || field.value.toLocaleLowerCase(i18n.language).includes(normalizedSearch)
      ))
      .sort((a, b) => {
        const hitsOrder = sortDirection === "desc" ? b.hits - a.hits : a.hits - b.hits;
        return hitsOrder || a.value.localeCompare(b.value, i18n.language);
      });
  }, [allFields, i18n.language, search, sortDirection]);

  useEffect(() => {
    requestedFields.current.clear();
  }, [onRequestFieldValues]);

  useEffect(() => {
    if (!open) return;
    for (const field of allFields) {
      const fieldName = field.value;
      if (
        !expandedFields.has(fieldName)
        || fieldValues[fieldName] !== undefined
        || fieldValuesLoading[fieldName]
        || fieldValuesErrors[fieldName]
        || requestedFields.current.has(fieldName)
      ) {
        continue;
      }
      requestedFields.current.add(fieldName);
      onRequestFieldValues(fieldName);
    }
  }, [
    allFields,
    expandedFields,
    fieldValues,
    fieldValuesErrors,
    fieldValuesLoading,
    onRequestFieldValues,
    open,
  ]);

  useEffect(() => {
    const syncStoredWidth = (event: StorageEvent) => {
      if (event.key !== SIDEBAR_WIDTH_STORAGE_KEY) return;
      const next = parseStoredSidebarWidth(event.newValue);
      currentWidth.current = next ?? DEFAULT_SIDEBAR_WIDTH;
      setSidebarWidth(next);
    };
    window.addEventListener("storage", syncStoredWidth);
    return () => window.removeEventListener("storage", syncStoredWidth);
  }, []);

  useEffect(() => {
    if (!isResizing) return;

    const previousCursor = document.body.style.cursor;
    const previousUserSelect = document.body.style.userSelect;
    document.body.style.cursor = "col-resize";
    document.body.style.userSelect = "none";

    const handlePointerMove = (event: PointerEvent) => {
      event.preventDefault();
      const dragOffset = event.clientX - resizeStart.current.pointerX;
      const next = clampSidebarWidth(resizeStart.current.width + dragOffset);
      currentWidth.current = next;
      setSidebarWidth(next);
    };
    const finishResize = () => {
      persistSidebarWidth(currentWidth.current);
      setIsResizing(false);
    };

    window.addEventListener("pointermove", handlePointerMove);
    window.addEventListener("pointerup", finishResize);
    window.addEventListener("pointercancel", finishResize);
    return () => {
      document.body.style.cursor = previousCursor;
      document.body.style.userSelect = previousUserSelect;
      window.removeEventListener("pointermove", handlePointerMove);
      window.removeEventListener("pointerup", finishResize);
      window.removeEventListener("pointercancel", finishResize);
    };
  }, [isResizing]);

  const resetSidebarWidth = () => {
    currentWidth.current = DEFAULT_SIDEBAR_WIDTH;
    setSidebarWidth(undefined);
    persistSidebarWidth(undefined);
  };

  const startSidebarResize = (event: ReactPointerEvent<HTMLDivElement>) => {
    if (event.button !== 0 || !sidebarRef.current) return;
    event.preventDefault();
    event.stopPropagation();
    resizeStart.current = {
      pointerX: event.clientX,
      width: sidebarRef.current.getBoundingClientRect().width,
    };
    currentWidth.current = resizeStart.current.width;
    setIsResizing(true);
  };

  const resizeSidebarWithKeyboard = (event: ReactKeyboardEvent<HTMLDivElement>) => {
    if (!["ArrowLeft", "ArrowRight", "Home"].includes(event.key)) return;
    event.preventDefault();
    if (event.key === "Home") {
      resetSidebarWidth();
      return;
    }
    const direction = event.key === "ArrowRight" ? 1 : -1;
    const renderedWidth = sidebarRef.current?.getBoundingClientRect().width
      ?? sidebarWidth
      ?? DEFAULT_SIDEBAR_WIDTH;
    const next = clampSidebarWidth(
      renderedWidth + direction * SIDEBAR_KEYBOARD_STEP,
    );
    currentWidth.current = next;
    setSidebarWidth(next);
    persistSidebarWidth(next);
  };
  const allVisibleFieldsExpanded = visibleFields.length > 0
    && visibleFields.every((field) => expandedFields.has(field.value));

  const toggleAllVisibleFields = () => {
    setExpandedFields((current) => {
      const next = new Set(current);
      for (const field of visibleFields) {
        if (allVisibleFieldsExpanded) next.delete(field.value);
        else next.add(field.value);
      }
      return next;
    });
  };

  if (!open) return null;

  return (
    <>
      <button
        type="button"
        className="fixed inset-0 z-40 bg-foreground/20 backdrop-blur-[1px] lg:hidden"
        aria-label={t("logs.closeFilters")}
        onClick={onClose}
      />
      <aside
        ref={sidebarRef}
        style={sidebarStyle}
        className="fixed inset-y-0 left-0 z-50 flex w-[min(22rem,calc(100vw-2rem))] flex-col border-r border-border bg-background shadow-xl lg:sticky lg:inset-x-auto lg:bottom-auto lg:top-4 lg:z-auto lg:h-[calc(100vh-6rem)] lg:w-[var(--logs-filter-sidebar-width)] lg:min-w-[250px] lg:max-w-[calc(100vw-250px)] lg:shrink-0 lg:rounded-lg lg:border lg:shadow-sm"
        aria-label={t("logs.streamFields")}
      >
        {fieldsLoading && <div className="h-0.5 animate-pulse bg-primary" />}
        <header className="flex items-center gap-2 border-b border-border p-3">
          <div className="min-w-0 flex-1">
            <h2 className="text-sm font-semibold">{t("logs.streamFields")}</h2>
            {filters.length > 0 && (
              <p className="mt-0.5 text-xs text-muted-foreground">
                {t("logs.selectedFilters", { count: filters.length })}
              </p>
            )}
          </div>
          {filters.length > 0 && (
            <Button
              type="button"
              variant="ghost"
              size="sm"
              className="h-8 px-2 text-xs text-muted-foreground"
              onClick={onClearFilters}
            >
              {t("logs.clearFilters")}
            </Button>
          )}
          <Button
            type="button"
            variant="ghost"
            size="icon"
            className="hidden size-8 lg:inline-flex"
            title={t("logs.resetFilterSidebarWidth")}
            aria-label={t("logs.resetFilterSidebarWidth")}
            onClick={resetSidebarWidth}
          >
            <RotateCcw className="size-4" aria-hidden="true" />
          </Button>
          <Button
            type="button"
            variant="ghost"
            size="icon"
            className="size-8"
            aria-label={t("logs.closeFilters")}
            onClick={onClose}
          >
            <X className="size-4" aria-hidden="true" />
          </Button>
        </header>

        <div className="flex items-center gap-1.5 border-b border-border p-2">
          <div className="relative min-w-0 flex-1">
            <Search
              className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground"
              aria-hidden="true"
            />
            <Input
              value={search}
              className="h-8 pl-8 pr-8"
              placeholder={t("logs.searchFields")}
              aria-label={t("logs.searchFields")}
              onChange={(event) => setSearch(event.target.value)}
            />
            {search && (
              <button
                type="button"
                className="absolute right-1.5 top-1/2 flex size-5 -translate-y-1/2 items-center justify-center rounded text-muted-foreground hover:bg-accent hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                aria-label={t("logs.clearFieldSearch")}
                onClick={() => setSearch("")}
              >
                <X className="size-3.5" aria-hidden="true" />
              </button>
            )}
          </div>
          <Button
            type="button"
            variant="outline"
            size="icon"
            className="size-8"
            aria-label={
              sortDirection === "desc"
                ? t("logs.sortFieldsAscending")
                : t("logs.sortFieldsDescending")
            }
            onClick={() => setSortDirection((current) => current === "desc" ? "asc" : "desc")}
          >
            <ArrowDownUp className="size-4" aria-hidden="true" />
          </Button>
          <Button
            type="button"
            variant="outline"
            size="icon"
            className="size-8"
            disabled={visibleFields.length === 0}
            aria-label={allVisibleFieldsExpanded ? t("logs.collapseAll") : t("logs.expandAll")}
            title={allVisibleFieldsExpanded ? t("logs.collapseAll") : t("logs.expandAll")}
            onClick={toggleAllVisibleFields}
          >
            {allVisibleFieldsExpanded ? (
              <ChevronsDownUp className="size-4" aria-hidden="true" />
            ) : (
              <ChevronsUpDown className="size-4" aria-hidden="true" />
            )}
          </Button>
        </div>

        <div className="min-h-0 flex-1 overflow-y-auto p-2">
          {fieldsError && (
            <div className="mb-2 rounded-md bg-destructive/10 p-2.5 text-xs text-destructive" role="alert">
              <span className="font-medium">{t("logs.streamFieldsLoadError")}</span>
              <span className="mt-0.5 block break-words">{fieldsError}</span>
            </div>
          )}
          {fieldsLoading && fields.length === 0 && (
            <div
              className="flex items-center gap-2 px-2 py-6 text-xs text-muted-foreground"
              role="status"
            >
              <LoaderCircle className="size-4 animate-spin" aria-hidden="true" />
              {t("logs.loadingStreamFields")}
            </div>
          )}
          {!fieldsLoading && !fieldsError && visibleFields.length === 0 && (
            <p className="px-2 py-6 text-center text-xs text-muted-foreground">
              {search ? t("logs.noMatchingFields") : t("logs.noStreamFields")}
            </p>
          )}
          <ul
            role="tree"
            aria-label={t("logs.streamFields")}
            className="space-y-0.5 text-xs"
          >
            {visibleFields.map((field) => (
              <FilterField
                key={field.value}
                field={field}
                expanded={expandedFields.has(field.value)}
                values={fieldValues[field.value]}
                valuesLoading={fieldValuesLoading[field.value] ?? false}
                valuesError={fieldValuesErrors[field.value]}
                filters={filters}
                sortDirection={sortDirection}
                numberFormatter={numberFormatter}
                onToggleExpanded={() => {
                  const opening = !expandedFields.has(field.value);
                  setExpandedFields((current) => {
                    const next = new Set(current);
                    if (next.has(field.value)) next.delete(field.value);
                    else next.add(field.value);
                    return next;
                  });
                  if (opening && fieldValues[field.value] === undefined) {
                    requestedFields.current.add(field.value);
                    onRequestFieldValues(field.value);
                  }
                }}
                onUpsertFilter={onUpsertFilter}
                onRemoveFilter={onRemoveFilter}
                onRemoveField={onRemoveField}
              />
            ))}
          </ul>
        </div>
        <div
          role="separator"
          tabIndex={0}
          aria-orientation="vertical"
          aria-label={t("logs.resizeFilterSidebar")}
          aria-valuemin={MIN_SIDEBAR_WIDTH}
          aria-valuemax={Math.max(MIN_SIDEBAR_WIDTH, window.innerWidth - MIN_SIDEBAR_WIDTH)}
          aria-valuenow={Math.round(clampSidebarWidth(sidebarWidth ?? DEFAULT_SIDEBAR_WIDTH))}
          className={cn(
            "group absolute -right-2.5 top-0 z-10 hidden h-full w-5 touch-none cursor-col-resize items-stretch justify-center outline-none lg:flex",
            "focus-visible:ring-2 focus-visible:ring-ring",
          )}
          title={t("logs.resizeFilterSidebar")}
          onPointerDown={startSidebarResize}
          onDoubleClick={resetSidebarWidth}
          onKeyDown={resizeSidebarWithKeyboard}
        >
          <span
            className={cn(
              "w-1 rounded-full transition-colors group-hover:bg-primary/40",
              isResizing && "bg-primary/50",
            )}
            aria-hidden="true"
          />
        </div>
      </aside>
      {isResizing && createPortal(
        <div
          className="fixed inset-0 z-[100] hidden cursor-col-resize lg:block"
          aria-hidden="true"
        />,
        document.body,
      )}
    </>
  );
}
