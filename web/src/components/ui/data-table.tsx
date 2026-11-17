import {
  flexRender,
  getCoreRowModel,
  getSortedRowModel,
  useReactTable,
  type ColumnDef,
  type Row,
  type SortingState,
  type TableOptions,
} from "@tanstack/react-table";
import { ArrowDown, ArrowUp, ChevronsUpDown } from "lucide-react";
import {
  useEffect,
  useRef,
  useState,
  type CSSProperties,
  type MouseEvent,
} from "react";
import { cn } from "@/lib/utils";

interface DataTableProps<TData> {
  columns: ColumnDef<TData>[];
  data: TData[];
  getRowId?: TableOptions<TData>["getRowId"];
  getRowAriaSelected?: (row: Row<TData>) => boolean | undefined;
  getRowClassName?: (row: Row<TData>) => string | undefined;
  getRowStyle?: (row: Row<TData>) => CSSProperties | undefined;
  numericColumnIds?: ReadonlySet<string>;
  onRowClick?: (row: Row<TData>, event: MouseEvent<HTMLTableRowElement>) => void;
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
export function DataTable<TData>({
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
  const pointerStartRef = useRef<{ x: number; y: number } | null>(null);
  const table = useReactTable({
    columns,
    data,
    getCoreRowModel: getCoreRowModel(),
    getRowId,
    getSortedRowModel: getSortedRowModel(),
    onSortingChange: setSorting,
    state: { sorting },
  });

  useEffect(() => {
    setSorting([]);
  }, [resetSortingKey]);

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
                          {flexRender(header.column.columnDef.header, header.getContext())}
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
              {row.getVisibleCells().map((cell) => {
                const numeric = numericColumnIds?.has(cell.column.id) === true;
                return (
                  <td
                    key={cell.id}
                    className={cn(
                      "overflow-hidden px-3 py-1.5 align-top",
                      numeric ? "text-right font-mono" : "text-left",
                    )}
                  >
                    {flexRender(cell.column.columnDef.cell, cell.getContext())}
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
