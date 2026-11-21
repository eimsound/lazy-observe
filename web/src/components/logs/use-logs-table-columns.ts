import {
  useRef,
  useState,
  type DragEvent,
  type KeyboardEvent,
  type PointerEvent,
} from "react";
import type { LogsTableViewSettings } from "./logs-explorer-model";
import {
  clampColumnWidth,
  columnWidth,
  moveColumnField,
  reorderColumns,
  type ColumnDropTarget,
} from "./logs-table-model";

interface ColumnResizeState {
  field: string;
  pointerId: number;
  startX: number;
  startWidth: number;
}

export function useLogsTableColumns(
  settings: LogsTableViewSettings,
  patchSettings: (patch: Partial<LogsTableViewSettings>) => void,
) {
  const [draggedField, setDraggedField] = useState<string | null>(null);
  const [dropTarget, setDropTarget] = useState<ColumnDropTarget | null>(null);
  const resizeRef = useRef<ColumnResizeState | null>(null);

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
    const visibleFields = moveColumnField(settings.visibleFields, field, offset);
    if (visibleFields) patchSettings({ visibleFields });
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

  return {
    draggedField,
    dropTarget,
    resetColumnWidth,
    moveColumn,
    handleColumnDragStart,
    handleColumnDragOver,
    handleColumnDrop,
    handleColumnDragEnd,
    handleResizeStart,
    handleResizeMove,
    handleResizeEnd,
    handleResizeKeyDown,
  };
}
