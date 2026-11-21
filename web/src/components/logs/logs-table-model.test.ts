import assert from "node:assert/strict";
import { test } from "vitest";
import type { LogRow } from "@/lib/api/logs";
import {
  MAX_COLUMN_WIDTH,
  MIN_COLUMN_WIDTH,
  clampColumnWidth,
  columnWidth,
  compareLogRows,
  defaultColumnWidth,
  moveColumnField,
  nextTableSort,
  reorderColumns,
  sortLogRows,
} from "./logs-table-model";

function makeRow(id: string, time: number, raw: Record<string, unknown>): LogRow {
  return {
    id,
    time,
    timeRaw: new Date(time).toISOString(),
    message: String(raw._msg ?? id),
    stream: String(raw._stream ?? "api"),
    level: "info",
    raw,
  };
}

test("column sizing applies defaults and clamps configured widths", () => {
  assert.equal(defaultColumnWidth("_time"), 200);
  assert.equal(defaultColumnWidth("_msg"), 420);
  assert.equal(defaultColumnWidth("_stream"), 260);
  assert.equal(defaultColumnWidth("service"), 190);

  assert.equal(clampColumnWidth(20), MIN_COLUMN_WIDTH);
  assert.equal(clampColumnWidth(2_000), MAX_COLUMN_WIDTH);
  assert.equal(clampColumnWidth(245.6), 246);

  assert.equal(columnWidth("_msg", undefined), 420);
  assert.equal(columnWidth("_msg", { _msg: 50 }), MIN_COLUMN_WIDTH);
  assert.equal(columnWidth("_msg", { _msg: Number.NaN }), 420);
  assert.equal(columnWidth("service", { service: 310 }), 310);
});

test("column reordering and keyboard shifting preserve valid ordering", () => {
  const fields = ["_time", "_msg", "_stream", "service"];
  assert.deepEqual(
    reorderColumns(fields, "service", "_msg", "before"),
    ["_time", "service", "_msg", "_stream"],
  );
  assert.deepEqual(
    reorderColumns(fields, "_time", "_stream", "after"),
    ["_msg", "_stream", "_time", "service"],
  );
  assert.deepEqual(reorderColumns(fields, "_msg", "_msg", "before"), fields);
  assert.deepEqual(reorderColumns(fields, "unknown", "_msg", "before"), fields);

  assert.equal(moveColumnField(fields, "_time", -1), null);
  assert.equal(moveColumnField(fields, "service", 1), null);
  assert.deepEqual(
    moveColumnField(fields, "_msg", 1),
    ["_time", "_stream", "_msg", "service"],
  );
});

test("table sort transitions cycle asc -> desc -> null and sort stably", () => {
  assert.deepEqual(nextTableSort(null, "status"), { field: "status", direction: "asc" });
  assert.deepEqual(
    nextTableSort({ field: "status", direction: "asc" }, "status"),
    { field: "status", direction: "desc" },
  );
  assert.equal(nextTableSort({ field: "status", direction: "desc" }, "status"), null);
  assert.deepEqual(
    nextTableSort({ field: "status", direction: "desc" }, "service"),
    { field: "service", direction: "asc" },
  );

  const rows = [
    makeRow("r1", 200, { status: 500, host: "node-10" }),
    makeRow("r2", 100, { status: 200, host: "node-2" }),
    makeRow("r3", 300, { host: "node-1" }),
    makeRow("r4", 150, { status: 200, host: "node-2" }),
  ];

  assert.equal(compareLogRows(rows[0], rows[1], "_time") > 0, true);
  assert.deepEqual(
    sortLogRows(rows, { field: "status", direction: "asc" }).map((row) => row.id),
    ["r2", "r4", "r1", "r3"],
  );
  assert.deepEqual(
    sortLogRows(rows, { field: "host", direction: "asc" }).map((row) => row.id),
    ["r3", "r2", "r4", "r1"],
  );
});
