import type { LogRow } from "@/lib/api/logs";
import {
  getRowField,
  stringifyFieldValue,
  type LogsTableSort,
} from "./logs-explorer-model";

export const MIN_COLUMN_WIDTH = 96;
export const MAX_COLUMN_WIDTH = 800;
export const TABLE_ACTIONS_WIDTH = 116;

export interface ColumnDropTarget {
  field: string;
  position: "before" | "after";
}

export function defaultColumnWidth(field: string): number {
  if (field === "_time") return 200;
  if (field === "_msg") return 420;
  if (field === "_stream") return 260;
  return 190;
}

export function clampColumnWidth(width: number): number {
  return Math.min(MAX_COLUMN_WIDTH, Math.max(MIN_COLUMN_WIDTH, Math.round(width)));
}

export function columnWidth(
  field: string,
  widths: Record<string, number> | undefined,
): number {
  const configured = widths?.[field];
  return typeof configured === "number" && Number.isFinite(configured)
    ? clampColumnWidth(configured)
    : defaultColumnWidth(field);
}

export function reorderColumns(
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

export function moveColumnField(
  fields: ReadonlyArray<string>,
  field: string,
  offset: -1 | 1,
): string[] | null {
  const from = fields.indexOf(field);
  const to = from + offset;
  if (from < 0 || to < 0 || to >= fields.length) return null;
  const visibleFields = [...fields];
  [visibleFields[from], visibleFields[to]] = [
    visibleFields[to],
    visibleFields[from],
  ];
  return visibleFields;
}

export function nextTableSort(
  current: LogsTableSort | null,
  field: string,
): LogsTableSort | null {
  if (!current || current.field !== field) {
    return { field, direction: "asc" };
  }
  if (current.direction === "asc") {
    return { field, direction: "desc" };
  }
  return null;
}

export function compareLogRows(left: LogRow, right: LogRow, field: string): number {
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

export function sortLogRows(rows: LogRow[], sort: LogsTableSort | null): LogRow[] {
  if (!sort) return rows;
  const { field, direction } = sort;
  const multiplier = direction === "asc" ? 1 : -1;

  return rows
    .map((row, index) => ({ row, index }))
    .sort((left, right) => {
      const comparison = compareLogRows(left.row, right.row, field);
      return comparison === 0
        ? left.index - right.index
        : comparison * multiplier;
    })
    .map(({ row }) => row);
}
