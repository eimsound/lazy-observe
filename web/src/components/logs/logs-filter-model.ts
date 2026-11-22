import type { LogsFieldValue } from "@/lib/api/logs";
import type { LogsExtraFilter } from "@/lib/logs-explorer";

export type FilterSortDirection = "asc" | "desc";

export interface FieldFilterSummary {
  fieldFilters: LogsExtraFilter[];
  anyFilter: LogsExtraFilter | undefined;
  allListedValuesSelected: boolean;
  count: number;
  badgeKind: "none" | "any" | "all" | "count";
}

export function mergeFilterFields(
  fields: readonly LogsFieldValue[],
  filters: readonly LogsExtraFilter[],
): LogsFieldValue[] {
  const combined = [...fields];
  const knownFields = new Set(fields.map((field) => field.value));
  for (const filter of filters) {
    if (!filter.isStream || knownFields.has(filter.field)) continue;
    combined.push({ value: filter.field, hits: 0 });
    knownFields.add(filter.field);
  }
  return combined;
}

export function filterAndSortFields(
  fields: readonly LogsFieldValue[],
  search: string,
  sortDirection: FilterSortDirection,
  locale?: string,
): LogsFieldValue[] {
  const normalizedSearch = search.trim().toLocaleLowerCase(locale);
  return fields
    .filter((field) => (
      !normalizedSearch
      || field.value.toLocaleLowerCase(locale).includes(normalizedSearch)
    ))
    .sort((a, b) => {
      const hitsOrder = sortDirection === "desc" ? b.hits - a.hits : a.hits - b.hits;
      return hitsOrder || a.value.localeCompare(b.value, locale);
    });
}

export function appendSelectedValues(
  values: readonly LogsFieldValue[],
  fieldFilters: readonly LogsExtraFilter[],
): LogsFieldValue[] {
  const result = [...values];
  const knownValues = new Set(values.map((item) => item.value));
  for (const filter of fieldFilters) {
    if (filter.value === "" || knownValues.has(filter.value)) continue;
    result.push({ value: filter.value, hits: 0 });
    knownValues.add(filter.value);
  }
  return result;
}

export function sortFieldValues(
  values: readonly LogsFieldValue[],
  fieldFilters: readonly LogsExtraFilter[],
  sortDirection: FilterSortDirection,
  locale?: string,
): LogsFieldValue[] {
  const allValues = appendSelectedValues(values, fieldFilters);
  return allValues.sort((a, b) => {
    const hitsOrder = sortDirection === "desc" ? b.hits - a.hits : a.hits - b.hits;
    return hitsOrder || a.value.localeCompare(b.value, locale);
  });
}

export function getFieldFilterSummary(
  field: string,
  filters: readonly LogsExtraFilter[],
  values: readonly LogsFieldValue[] | undefined,
): FieldFilterSummary {
  const fieldFilters = filters.filter((filter) => (
    filter.isStream && filter.field === field
  ));
  const anyFilter = fieldFilters.find((filter) => (
    filter.value === "" && filter.operator === "!="
  ));
  const allListedValuesSelected = Boolean(
    values?.length
    && values.every((item) => fieldFilters.some((filter) => filter.value === item.value)),
  );
  const badgeKind: FieldFilterSummary["badgeKind"] = fieldFilters.length === 0
    ? "none"
    : anyFilter
      ? "any"
      : allListedValuesSelected
        ? "all"
        : "count";
  return {
    fieldFilters,
    anyFilter,
    allListedValuesSelected,
    count: fieldFilters.length,
    badgeKind,
  };
}

export function resolveActiveField(
  selectedField: string | null,
  visibleFields: readonly LogsFieldValue[],
): string | null {
  if (selectedField !== null && visibleFields.some((field) => field.value === selectedField)) {
    return selectedField;
  }
  return visibleFields[0]?.value ?? null;
}

export function resolveNextFieldIndex(
  currentIndex: number,
  count: number,
  key: string,
): number | null {
  if (count <= 0) return null;
  switch (key) {
    case "ArrowDown":
      return currentIndex < 0 ? 0 : Math.min(currentIndex + 1, count - 1);
    case "ArrowUp":
      return currentIndex < 0 ? count - 1 : Math.max(currentIndex - 1, 0);
    case "Home":
      return 0;
    case "End":
      return count - 1;
    default:
      return null;
  }
}
