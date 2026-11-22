import { useEffect, useMemo, useRef, useState, type KeyboardEvent } from "react";
import { useTranslation } from "react-i18next";
import {
  ArrowDownUp,
  Check,
  Filter,
  LoaderCircle,
  Minus,
  RotateCcw,
  Search,
  X,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import type { LogsFieldValue } from "@/lib/api/logs";
import type { LogsExtraFilter } from "@/lib/logs-explorer";
import { cn } from "@/lib/utils";
import {
  filterAndSortFields,
  getFieldFilterSummary,
  mergeFilterFields,
  resolveActiveField,
  resolveNextFieldIndex,
  sortFieldValues,
  type FilterSortDirection,
} from "./logs-filter-model";

export interface LogsFilterPopoverProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  fields: LogsFieldValue[];
  fieldsLoading?: boolean;
  fieldsError?: string | null;
  /** Values are keyed by their field name. An absent key means they have not loaded yet. */
  fieldValues: Readonly<Record<string, LogsFieldValue[] | undefined>>;
  fieldValuesLoading?: Readonly<Record<string, boolean | undefined>>;
  fieldValuesErrors?: Readonly<Record<string, string | null | undefined>>;
  filters: LogsExtraFilter[];
  onRetryFields?: () => void;
  onRequestFieldValues: (field: string) => void;
  onUpsertFilter: (filter: LogsExtraFilter) => void;
  onRemoveFilter: (field: string, value: string) => void;
  onRemoveField: (field: string) => void;
  onClearFilters: () => void;
}

interface FilterValueRowProps {
  field: string;
  item: LogsFieldValue;
  filter?: LogsExtraFilter;
  implicitlySelected: boolean;
  numberFormatter: Intl.NumberFormat;
  onUpsertFilter: (filter: LogsExtraFilter) => void;
  onRemoveFilter: (field: string, value: string) => void;
}

function FilterValueRow({
  field,
  item,
  filter,
  implicitlySelected,
  numberFormatter,
  onUpsertFilter,
  onRemoveFilter,
}: FilterValueRowProps) {
  const { t } = useTranslation();
  const selected = Boolean(filter);
  const checked = selected || implicitlySelected;
  const included = filter?.operator === "=" || filter?.operator === "=~";
  const excluded = filter?.operator === "!=" || filter?.operator === "!~";
  const valueLabel = item.value || t("logs.emptyValue");

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
        "flex min-w-0 items-center gap-1 rounded-md border border-transparent pl-2 transition-colors",
        selected ? "bg-primary/10 text-foreground" : "text-muted-foreground hover:bg-accent/70",
        item.hits === 0 && "opacity-75",
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
            selected && excluded
              ? "border-destructive bg-destructive text-destructive-foreground"
              : checked
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
          {valueLabel}
        </span>
        <span className="shrink-0 tabular-nums text-muted-foreground">
          {numberFormatter.format(item.hits)}
        </span>
      </button>

      <div className="flex shrink-0 items-center gap-0.5 pr-1">
        <button
          type="button"
          className={cn(
            "inline-flex h-6 min-w-6 items-center justify-center rounded px-1.5 font-mono text-[11px] transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
            included
              ? "bg-primary text-primary-foreground"
              : "text-muted-foreground hover:bg-accent hover:text-foreground",
          )}
          aria-pressed={included}
          aria-label={`${t("logs.includeValue")} ${valueLabel}`}
          title={t("logs.includeValue")}
          onClick={() => {
            if (included) onRemoveFilter(field, item.value);
            else upsert("=");
          }}
        >
          =
        </button>
        <button
          type="button"
          className={cn(
            "inline-flex h-6 min-w-6 items-center justify-center rounded px-1.5 font-mono text-[11px] transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
            excluded
              ? "bg-destructive text-destructive-foreground"
              : "text-muted-foreground hover:bg-accent hover:text-foreground",
          )}
          aria-pressed={excluded}
          aria-label={`${t("logs.excludeValue")} ${valueLabel}`}
          title={t("logs.excludeValue")}
          onClick={() => {
            if (excluded) onRemoveFilter(field, item.value);
            else upsert("!=");
          }}
        >
          !=
        </button>
      </div>
    </div>
  );
}

export function LogsFilterPopover({
  open,
  onOpenChange,
  fields,
  fieldsLoading = false,
  fieldsError,
  fieldValues,
  fieldValuesLoading = {},
  fieldValuesErrors = {},
  filters,
  onRetryFields,
  onRequestFieldValues,
  onUpsertFilter,
  onRemoveFilter,
  onRemoveField,
  onClearFilters,
}: LogsFilterPopoverProps) {
  const { t, i18n } = useTranslation();
  const [search, setSearch] = useState("");
  const [sortDirection, setSortDirection] = useState<FilterSortDirection>("desc");
  const [selectedField, setSelectedField] = useState<string | null>(null);
  const requestedFields = useRef(new Set<string>());
  const fieldOptionRefs = useRef(new Map<string, HTMLButtonElement>());

  const numberFormatter = useMemo(
    () => new Intl.NumberFormat(i18n.language, { notation: "compact", maximumFractionDigits: 1 }),
    [i18n.language],
  );

  const streamFilters = useMemo(
    () => filters.filter((filter) => filter.isStream),
    [filters],
  );

  const allFields = useMemo(
    () => mergeFilterFields(fields, streamFilters),
    [fields, streamFilters],
  );

  const visibleFields = useMemo(
    () => filterAndSortFields(allFields, search, sortDirection, i18n.language),
    [allFields, i18n.language, search, sortDirection],
  );

  const activeField = resolveActiveField(selectedField, visibleFields);

  useEffect(() => {
    requestedFields.current.clear();
  }, [onRequestFieldValues]);

  useEffect(() => {
    if (!open || !activeField) return;
    if (
      fieldValues[activeField] !== undefined
      || fieldValuesLoading[activeField]
      || fieldValuesErrors[activeField]
      || requestedFields.current.has(activeField)
    ) {
      return;
    }
    requestedFields.current.add(activeField);
    onRequestFieldValues(activeField);
  }, [
    activeField,
    fieldValues,
    fieldValuesErrors,
    fieldValuesLoading,
    onRequestFieldValues,
    open,
  ]);

  const activeValues = activeField ? fieldValues[activeField] : undefined;
  const activeValuesLoading = activeField ? (fieldValuesLoading[activeField] ?? false) : false;
  const activeValuesError = activeField ? fieldValuesErrors[activeField] : undefined;

  const activeSummary = useMemo(
    () => (
      activeField
        ? getFieldFilterSummary(activeField, streamFilters, activeValues)
        : null
    ),
    [activeField, activeValues, streamFilters],
  );

  const listedValues = useMemo(
    () => (
      activeSummary
        ? sortFieldValues(activeValues ?? [], activeSummary.fieldFilters, sortDirection, i18n.language)
        : []
    ),
    [activeSummary, activeValues, i18n.language, sortDirection],
  );

  const handleRetryFields = () => {
    if (!onRetryFields || fieldsLoading) return;
    onRetryFields();
  };

  const handleRetryFieldValues = () => {
    if (!activeField || activeValuesLoading) return;
    requestedFields.current.add(activeField);
    onRequestFieldValues(activeField);
  };

  const focusFieldOption = (field: string) => {
    setSelectedField(field);
    const button = fieldOptionRefs.current.get(field);
    if (!button) return;
    button.focus();
    button.scrollIntoView({ block: "nearest" });
  };

  const handleSearchKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    if (
      event.nativeEvent.isComposing
      || event.altKey
      || event.ctrlKey
      || event.metaKey
      || event.key !== "ArrowDown"
      || visibleFields.length === 0
    ) {
      return;
    }
    event.preventDefault();
    focusFieldOption(activeField ?? visibleFields[0].value);
  };

  const handleFieldOptionKeyDown = (
    event: KeyboardEvent<HTMLButtonElement>,
    index: number,
  ) => {
    if (
      event.nativeEvent.isComposing
      || event.altKey
      || event.ctrlKey
      || event.metaKey
    ) {
      return;
    }
    const nextIndex = resolveNextFieldIndex(index, visibleFields.length, event.key);
    if (nextIndex === null) return;
    event.preventDefault();
    focusFieldOption(visibleFields[nextIndex].value);
  };

  return (
    <Popover open={open} onOpenChange={onOpenChange}>
      <PopoverTrigger asChild>
        <Button
          type="button"
          variant={open || streamFilters.length > 0 ? "secondary" : "outline"}
          size="sm"
          className="h-9 gap-1.5 text-sm"
        >
          <Filter className="size-4" aria-hidden="true" />
          <span>{t("logs.filters")}</span>
          {streamFilters.length > 0 && (
            <span className="inline-flex min-w-5 items-center justify-center rounded-full bg-primary px-1.5 py-0.5 text-[10px] font-semibold leading-none tabular-nums text-primary-foreground">
              {streamFilters.length}
            </span>
          )}
        </Button>
      </PopoverTrigger>
      <PopoverContent
        align="start"
        collisionPadding={12}
        className="w-[min(36rem,calc(100vw-2rem))] overflow-hidden p-0"
        aria-label={t("logs.filters")}
      >
        {fieldsLoading && <div className="h-0.5 animate-pulse bg-primary" />}
        <header className="flex items-center gap-2 border-b border-border px-3 py-2.5">
          <h2 className="text-sm font-semibold">{t("logs.filters")}</h2>
          {streamFilters.length > 0 && (
            <span className="inline-flex items-center rounded-full bg-primary/10 px-2 py-0.5 text-xs font-medium tabular-nums text-primary">
              {t("logs.selectedFilters", { count: streamFilters.length })}
            </span>
          )}
          <div className="ml-auto flex items-center gap-1">
            {streamFilters.length > 0 && (
              <Button
                type="button"
                variant="ghost"
                size="sm"
                className="h-7 px-2 text-xs text-muted-foreground"
                onClick={onClearFilters}
              >
                {t("logs.clearFilters")}
              </Button>
            )}
            <Button
              type="button"
              variant="ghost"
              size="icon"
              className="size-7"
              aria-label={t("logs.closeFilters")}
              onClick={() => onOpenChange(false)}
            >
              <X className="size-4" aria-hidden="true" />
            </Button>
          </div>
        </header>

        <div className="grid grid-cols-1 divide-y divide-border sm:grid-cols-[minmax(11rem,13.5rem)_minmax(0,1fr)] sm:divide-x sm:divide-y-0">
          <div className="flex h-48 max-h-[30vh] min-w-0 flex-col sm:h-72 sm:max-h-none">
            <div className="flex shrink-0 items-center gap-1.5 border-b border-border p-2">
              <div className="relative min-w-0 flex-1">
                <Search
                  className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground"
                  aria-hidden="true"
                />
                <Input
                  value={search}
                  className="h-8 pl-8 pr-7 text-xs"
                  placeholder={t("logs.searchFields")}
                  aria-label={t("logs.searchFields")}
                  onChange={(event) => setSearch(event.target.value)}
                  onKeyDown={handleSearchKeyDown}
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
                className="size-8 shrink-0"
                aria-label={
                  sortDirection === "desc"
                    ? t("logs.sortFieldsAscending")
                    : t("logs.sortFieldsDescending")
                }
                title={
                  sortDirection === "desc"
                    ? t("logs.sortFieldsAscending")
                    : t("logs.sortFieldsDescending")
                }
                onClick={() => setSortDirection((current) => current === "desc" ? "asc" : "desc")}
              >
                <ArrowDownUp className="size-3.5" aria-hidden="true" />
              </Button>
            </div>

            <div className="min-h-0 flex-1 overflow-y-auto p-1.5">
              {fieldsError && (
                <div className="mb-2 rounded-md bg-destructive/10 p-2.5 text-xs text-destructive" role="alert">
                  <span className="font-medium">{t("logs.filterFieldsLoadError")}</span>
                  <span className="mt-0.5 block break-words">{fieldsError}</span>
                  {onRetryFields && (
                    <Button
                      type="button"
                      variant="outline"
                      size="sm"
                      className="mt-2 h-7 gap-1.5 px-2 text-xs text-foreground [&_svg]:size-3.5"
                      disabled={fieldsLoading}
                      onClick={handleRetryFields}
                    >
                      <RotateCcw aria-hidden="true" />
                      {t("common.retry")}
                    </Button>
                  )}
                </div>
              )}
              {fieldsLoading && fields.length === 0 && (
                <div
                  className="flex items-center gap-2 px-2 py-6 text-xs text-muted-foreground"
                  role="status"
                >
                  <LoaderCircle className="size-4 animate-spin" aria-hidden="true" />
                  {t("logs.loadingFilterFields")}
                </div>
              )}
              {!fieldsLoading && !fieldsError && visibleFields.length === 0 && (
                <p className="px-2 py-6 text-center text-xs text-muted-foreground">
                  {search ? t("logs.noMatchingFields") : t("logs.noFilterFields")}
                </p>
              )}
              {visibleFields.length > 0 && (
                <div role="listbox" aria-label={t("logs.filterFields")} className="space-y-0.5 text-xs">
                  {visibleFields.map((field, index) => {
                    const isActive = field.value === activeField;
                    const summary = getFieldFilterSummary(
                      field.value,
                      streamFilters,
                      fieldValues[field.value],
                    );
                    const badgeLabel = summary.badgeKind === "any"
                      ? t("logs.anyValue")
                      : summary.badgeKind === "all"
                        ? t("logs.allValues")
                        : summary.count;
                    const badgeTitle = summary.badgeKind === "any"
                      ? t("logs.anyNonEmptyValue")
                      : summary.badgeKind === "all"
                        ? t("logs.allListedValues")
                        : t("logs.selectedValues", { count: summary.count });

                    return (
                      <button
                        key={field.value}
                        ref={(node) => {
                          if (node) fieldOptionRefs.current.set(field.value, node);
                          else fieldOptionRefs.current.delete(field.value);
                        }}
                        type="button"
                        role="option"
                        aria-selected={isActive}
                        tabIndex={isActive ? 0 : -1}
                        className={cn(
                          "flex w-full min-w-0 items-center gap-1.5 rounded-md px-2 py-1.5 text-left transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                          isActive
                            ? "bg-accent font-medium text-accent-foreground"
                            : summary.badgeKind !== "none"
                              ? "bg-primary/[0.06] text-foreground hover:bg-accent/70"
                              : "text-foreground hover:bg-accent/70",
                          field.hits === 0 && "opacity-80",
                        )}
                        onClick={() => setSelectedField(field.value)}
                        onKeyDown={(event) => handleFieldOptionKeyDown(event, index)}
                      >
                        <span className="min-w-0 flex-1 truncate font-mono" title={field.value}>
                          {field.value}
                        </span>
                        <span className="shrink-0 tabular-nums text-muted-foreground">
                          {numberFormatter.format(field.hits)}
                        </span>
                        {summary.badgeKind !== "none" && (
                          <span
                            className="flex min-w-5 shrink-0 items-center justify-center rounded-full bg-primary px-1.5 py-0.5 text-[10px] font-semibold leading-none text-primary-foreground"
                            title={badgeTitle}
                          >
                            {badgeLabel}
                          </span>
                        )}
                      </button>
                    );
                  })}
                </div>
              )}
            </div>
          </div>

          <div className="flex h-52 max-h-[34vh] min-w-0 flex-col sm:h-72 sm:max-h-none">
            {!activeField || !activeSummary ? (
              <p className="px-3 py-8 text-center text-xs text-muted-foreground">
                {fieldsLoading ? t("logs.loadingFilterFields") : t("logs.selectFilterFieldHint")}
              </p>
            ) : (
              <>
                <div className="flex shrink-0 items-center gap-1.5 border-b border-border bg-muted/20 px-2.5 py-2">
                  <span
                    className="min-w-0 flex-1 truncate font-mono text-xs font-semibold"
                    title={activeField}
                  >
                    {activeField}
                  </span>
                  <Button
                    type="button"
                    variant={activeSummary.anyFilter ? "secondary" : "outline"}
                    size="sm"
                    className="h-6 gap-1 px-2 text-[11px]"
                    aria-pressed={Boolean(activeSummary.anyFilter)}
                    title={
                      activeSummary.anyFilter
                        ? t("logs.anyNonEmptyValue")
                        : t("logs.filterAnyValue", { field: activeField })
                    }
                    onClick={() => {
                      if (activeSummary.anyFilter) {
                        onRemoveFilter(activeField, "");
                        return;
                      }
                      onUpsertFilter({
                        field: activeField,
                        value: "",
                        operator: "!=",
                        isStream: true,
                      });
                    }}
                  >
                    {activeSummary.anyFilter && <Check className="size-3" aria-hidden="true" />}
                    {t("logs.anyValue")}
                  </Button>
                  {activeSummary.count > 0 && (
                    <Button
                      type="button"
                      variant="ghost"
                      size="sm"
                      className="h-6 px-2 text-[11px] text-muted-foreground"
                      aria-label={t("logs.clearFieldFilters", { field: activeField })}
                      title={t("logs.clearFieldFilters", { field: activeField })}
                      onClick={() => onRemoveField(activeField)}
                    >
                      {t("common.clear")}
                    </Button>
                  )}
                </div>

                <div className="min-h-0 flex-1 overflow-y-auto p-1.5">
                  {activeValuesLoading && (
                    <div
                      className="flex items-center gap-2 px-2 py-6 text-xs text-muted-foreground"
                      role="status"
                    >
                      <LoaderCircle className="size-3.5 animate-spin" aria-hidden="true" />
                      {t("logs.loadingFieldValues")}
                    </div>
                  )}
                  {activeValuesError && (
                    <div
                      className="rounded bg-destructive/10 px-2.5 py-2 text-xs text-destructive"
                      role="alert"
                    >
                      <span className="font-medium">{t("logs.fieldValuesLoadError")}</span>
                      <span className="mt-0.5 block break-words">{activeValuesError}</span>
                      <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        className="mt-2 h-7 gap-1.5 px-2 text-xs text-foreground [&_svg]:size-3.5"
                        disabled={activeValuesLoading}
                        onClick={handleRetryFieldValues}
                      >
                        <RotateCcw aria-hidden="true" />
                        {t("common.retry")}
                      </Button>
                    </div>
                  )}
                  {!activeValuesLoading
                    && !activeValuesError
                    && activeValues !== undefined
                    && listedValues.length === 0 && (
                      <p className="px-2 py-6 text-center text-xs text-muted-foreground">
                        {t("logs.noFieldValues")}
                      </p>
                  )}
                  {listedValues.length > 0 && (
                    <ul
                      className="space-y-0.5 text-xs"
                      aria-label={t("logs.filterValuesForField", { field: activeField })}
                    >
                      {listedValues.map((item) => {
                        const filter = activeSummary.fieldFilters.find(
                          (candidate) => candidate.value === item.value,
                        );
                        return (
                          <li key={item.value}>
                            <FilterValueRow
                              field={activeField}
                              item={item}
                              filter={filter}
                              implicitlySelected={Boolean(activeSummary.anyFilter)}
                              numberFormatter={numberFormatter}
                              onUpsertFilter={onUpsertFilter}
                              onRemoveFilter={onRemoveFilter}
                            />
                          </li>
                        );
                      })}
                    </ul>
                  )}
                </div>
              </>
            )}
          </div>
        </div>
      </PopoverContent>
    </Popover>
  );
}
