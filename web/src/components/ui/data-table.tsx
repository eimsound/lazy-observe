import {
  columnSizingFeature,
  createSortedRowModel,
  rowSortingFeature,
  sortFn_alphanumeric,
  sortFn_basic,
  sortFn_datetime,
  sortFn_text,
  tableFeatures,
  useTable,
  type ColumnDef,
  type Row,
  type RowData,
  type SortingState,
  type TableOptions,
} from "@tanstack/react-table";
import { ArrowDown, ArrowUp, ChevronsUpDown } from "lucide-react";
import {
  useMemo,
  useRef,
  useState,
  type CSSProperties,
  type MouseEvent,
} from "react";
import { v8AutoSortKind, v8SortDescFirst, type V8AutoSortKind } from "@/lib/table/v8-sort-parity";
import { cn } from "@/lib/utils";

// TanStack Table v9 only exposes the APIs of registered features: sorting for the
// header toggles and sizing for getTotalSize(). The core row model is built in.
const dataTableFeatures = tableFeatures({
  rowSortingFeature,
  columnSizingFeature,
  sortedRowModel: createSortedRowModel(),
});

export type DataTableFeatures = typeof dataTableFeatures;
export type DataTableColumnDef<TData extends RowData> = ColumnDef<DataTableFeatures, TData>;
export type DataTableRow<TData extends RowData> = Row<DataTableFeatures, TData>;

const V8_SORT_FNS: Record<V8AutoSortKind, typeof sortFn_basic> = {
  datetime: sortFn_datetime,
  alphanumeric: sortFn_alphanumeric,
  text: sortFn_text,
  basic: sortFn_basic,
};

function columnAccessor<TData extends RowData>(
  column: DataTableColumnDef<TData>,
): ((row: TData, index: number) => unknown) | undefined {
  if ("accessorFn" in column && column.accessorFn) return column.accessorFn;
  if ("accessorKey" in column && column.accessorKey !== undefined) {
    const path = String(column.accessorKey).split(".");
    return (row) => path.reduce<unknown>((value, key) => (value as Record<string, unknown> | undefined)?.[key], row);
  }
  return undefined;
}

/**
 * Pin each accessor column's default comparator and first sort direction to the
 * v8 heuristics (see v8-sort-parity.ts) so sorting behaves as it did under v8.
 * Values a caller sets explicitly still win.
 */
function withV8SortDefaults<TData extends RowData>(
  columns: DataTableColumnDef<TData>[],
  data: TData[],
): DataTableColumnDef<TData>[] {
  return columns.map((column) => {
    const accessor = columnAccessor(column);
    if (!accessor) return column;
    const values = data.map(accessor);
    return {
      sortFn: V8_SORT_FNS[v8AutoSortKind(values)],
      sortDescFirst: v8SortDescFirst(values),
      ...column,
    };
  });
}

interface DataTableProps<TData extends RowData> {
  columns: DataTableColumnDef<TData>[];
  data: TData[];
  getRowId?: TableOptions<DataTableFeatures, TData>["getRowId"];
  getRowAriaSelected?: (row: DataTableRow<TData>) => boolean | undefined;
  getRowClassName?: (row: DataTableRow<TData>) => string | undefined;
  getRowStyle?: (row: DataTableRow<TData>) => CSSProperties | undefined;
  numericColumnIds?: ReadonlySet<string>;
  onRowClick?: (row: DataTableRow<TData>, event: MouseEvent<HTMLTableRowElement>) => void;
  resetSortingKey?: unknown;
  sortButtonLabel: string;
  minWidth?: number;
  className?: string;
  tableClassName?: string;
}

/**
 * Shared TanStack Table renderer. It owns common sorting and pointer behavior,
 * while callers provide only their columns, rows, and optional row actions.
 */
export function DataTable<TData extends RowData>({
  columns,
  data,
  getRowId,
  getRowAriaSelected,
  getRowClassName,
  getRowStyle,
  numericColumnIds,
  onRowClick,
  resetSortingKey,
  sortButtonLabel,
  minWidth = 0,
  className,
  tableClassName,
}: DataTableProps<TData>) {
  const [sorting, setSorting] = useState<SortingState>([]);
  const [prevResetSortingKey, setPrevResetSortingKey] = useState(resetSortingKey);
  if (!Object.is(prevResetSortingKey, resetSortingKey)) {
    setPrevResetSortingKey(resetSortingKey);
    if (sorting.length > 0) {
      setSorting([]);
    }
  }
  const pointerStartRef = useRef<{ x: number; y: number } | null>(null);
  const tableColumns = useMemo(() => withV8SortDefaults(columns, data), [columns, data]);
  const table = useTable({
    features: dataTableFeatures,
    columns: tableColumns,
    data,
    getRowId,
    onSortingChange: setSorting,
    state: { sorting },
  });

  return (
    <div
      className={cn(
        "select-text overflow-auto rounded-md border border-border/70 text-xs",
        className,
      )}
      onMouseDownCapture={(event) => {
        if (event.ctrlKey || event.metaKey) {
          event.preventDefault();
        }
      }}
    >
      <table
        className={cn("w-full table-auto text-left", tableClassName)}
        style={{ minWidth: Math.max(minWidth, table.getTotalSize()) }}
      >
        <thead className="sticky top-0 z-10 bg-muted/95 text-muted-foreground backdrop-blur">
          {table.getHeaderGroups().map((headerGroup) => (
            <tr key={headerGroup.id} className="border-b border-border">
              {headerGroup.headers.map((header) => {
                const numeric = numericColumnIds?.has(header.column.id) === true;
                const sorted = header.column.getIsSorted();
                return (
                  <th
                    key={header.id}
                    className={cn(
                      "overflow-hidden px-3 py-2 font-medium",
                      numeric ? "text-right" : "text-left",
                    )}
                  >
                    {header.isPlaceholder ? null : (
                      <div
                        className={cn(
                          "flex min-w-0 w-full items-center gap-1",
                          numeric ? "justify-end" : "justify-start",
                        )}
                      >
                        <span className="min-w-0 break-all select-text">
                          <table.FlexRender header={header} />
                        </span>
                        {header.column.getCanSort() && (
                          <button
                            type="button"
                            className="rounded p-0.5 hover:bg-muted hover:text-foreground"
                            aria-label={sortButtonLabel}
                            onClick={header.column.getToggleSortingHandler()}
                          >
                            {sorted === "asc"
                              ? <ArrowUp className="size-3" />
                              : sorted === "desc"
                                ? <ArrowDown className="size-3" />
                                : <ChevronsUpDown className="size-3 opacity-45" />}
                          </button>
                        )}
                      </div>
                    )}
                  </th>
                );
              })}
            </tr>
          ))}
        </thead>
        <tbody>
          {table.getRowModel().rows.map((row) => (
            <tr
              key={row.id}
              className={cn(
                "border-b border-border/60 last:border-0",
                onRowClick && "cursor-pointer hover:bg-muted/50",
                getRowClassName?.(row),
              )}
              style={getRowStyle?.(row)}
              aria-selected={getRowAriaSelected?.(row)}
              onPointerDown={onRowClick
                ? (event) => {
                    pointerStartRef.current = { x: event.clientX, y: event.clientY };
                  }
                : undefined}
              onClick={onRowClick
                ? (event) => {
                    const start = pointerStartRef.current;
                    pointerStartRef.current = null;
                    if (
                      start
                      && (Math.abs(event.clientX - start.x) > 4
                        || Math.abs(event.clientY - start.y) > 4)
                    ) {
                      return;
                    }
                    onRowClick(row, event);
                  }
                : undefined}
            >
              {row.getAllCells().map((cell) => {
                const numeric = numericColumnIds?.has(cell.column.id) === true;
                return (
                  <td
                    key={cell.id}
                    className={cn(
                      "overflow-hidden px-3 py-1.5 align-top",
                      numeric ? "text-right font-mono" : "text-left",
                    )}
                  >
                    <table.FlexRender cell={cell} />
                  </td>
                );
              })}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
