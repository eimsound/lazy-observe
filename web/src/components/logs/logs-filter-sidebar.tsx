import { useEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { useTranslation } from "react-i18next";
import {
  ArrowDownUp,
  ChevronsDownUp,
  ChevronsUpDown,
  LoaderCircle,
  RotateCcw,
  Search,
  X,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import type { LogsFieldValue } from "@/lib/api/logs";
import type { LogsExtraFilter } from "@/lib/logs-explorer";
import { cn } from "@/lib/utils";
import {
  FilterFieldItem,
  type FilterSortDirection,
} from "./logs-filter-field-item";
import {
  DEFAULT_SIDEBAR_WIDTH,
  MIN_SIDEBAR_WIDTH,
  clampSidebarWidth,
  useResizableSidebar,
} from "./use-resizable-sidebar";

export { ExtraFiltersBar, type ExtraFiltersBarProps } from "./logs-extra-filters-bar";

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
  const [sortDirection, setSortDirection] = useState<FilterSortDirection>("desc");
  const [expandedFields, setExpandedFields] = useState<Set<string>>(() => new Set());
  const requestedFields = useRef(new Set<string>());
  const {
    sidebarRef,
    sidebarWidth,
    sidebarStyle,
    isResizing,
    resetSidebarWidth,
    startSidebarResize,
    resizeSidebarWithKeyboard,
  } = useResizableSidebar();
  const numberFormatter = useMemo(
    () => new Intl.NumberFormat(i18n.language, { notation: "compact", maximumFractionDigits: 1 }),
    [i18n.language],
  );

  const allFields = useMemo(() => {
    const combined = [...fields];
    const knownFields = new Set(fields.map((field) => field.value));
    for (const filter of filters) {
      if (!filter.isStream || knownFields.has(filter.field)) continue;
      combined.push({ value: filter.field, hits: 0 });
      knownFields.add(filter.field);
    }
    return combined;
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
              <FilterFieldItem
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
