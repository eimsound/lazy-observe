import { useMemo } from "react";
import { useTranslation } from "react-i18next";
import {
  Check,
  ChevronDown,
  ChevronRight,
  LoaderCircle,
  Minus,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import type { LogsFieldValue } from "@/lib/api/logs";
import type { LogsExtraFilter } from "@/lib/logs-explorer";
import { cn } from "@/lib/utils";

export type FilterSortDirection = "asc" | "desc";

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

export interface FilterFieldItemProps {
  field: LogsFieldValue;
  expanded: boolean;
  values: LogsFieldValue[] | undefined;
  valuesLoading: boolean;
  valuesError?: string | null;
  filters: LogsExtraFilter[];
  sortDirection: FilterSortDirection;
  numberFormatter: Intl.NumberFormat;
  onToggleExpanded: () => void;
  onUpsertFilter: (filter: LogsExtraFilter) => void;
  onRemoveFilter: (field: string, value: string) => void;
  onRemoveField: (field: string) => void;
}

export function FilterFieldItem({
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
}: FilterFieldItemProps) {
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
