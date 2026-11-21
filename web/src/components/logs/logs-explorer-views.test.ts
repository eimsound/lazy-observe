import assert from "node:assert/strict";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { test } from "vitest";
import type { LogRow } from "@/lib/api/logs";
import { buildStableLogRowKeys } from "./logs-explorer-model.ts";
import { LogsJsonView } from "./logs-explorer-views.tsx";

function makeLogs(count: number): LogRow[] {
  const rows: LogRow[] = [];
  for (let i = 0; i < count; i++) {
    const time = 1_700_000_000_000 + i * 10;
    const raw: Record<string, unknown> = {
      _time: new Date(time).toISOString(),
      _stream: `{service="api-gateway",instance="pod-${i % 8}"}`,
      _msg: `request handled /api/v1/orders/${i}`,
      level: i % 10 === 0 ? "error" : "info",
      seq: i,
    };
    rows.push({
      id: `log-${i}`,
      time,
      timeRaw: String(raw._time),
      stream: String(raw._stream),
      message: String(raw._msg),
      level: i % 10 === 0 ? "error" : "info",
      traceId: "",
      raw,
    });
  }
  return rows;
}

test("LogsJsonView renders empty state when rows is empty", () => {
  const html = renderToStaticMarkup(React.createElement(LogsJsonView, { rows: [] }));
  assert.equal((html.match(/role="listitem"/g) ?? []).length, 0);
  assert.match(html, /disabled=""/);
});

test("LogsJsonView renders all rows without virtualization for small datasets (<= 80 rows)", () => {
  const html = renderToStaticMarkup(React.createElement(LogsJsonView, { rows: makeLogs(25) }));
  const renderedRows = (html.match(/role="listitem"/g) ?? []).length;

  assert.equal(renderedRows, 25);
  assert.match(html, /data-virtualized="false"/);
  assert.match(html, /\/api\/v1\/orders\/0/);
  assert.match(html, /\/api\/v1\/orders\/24/);
  assert.match(html, /aria-expanded="false"/);
});

test("LogsJsonView virtualizes 2,000 and 5,000 rows to a constant bounded DOM window", () => {
  const html2k = renderToStaticMarkup(React.createElement(LogsJsonView, { rows: makeLogs(2_000) }));
  const rows2k = (html2k.match(/role="listitem"/g) ?? []).length;

  assert.match(html2k, /data-virtualized="true"/);
  // 640px default viewport / 24px compact row height = 27 visible + 12 overscan = 39 rows.
  assert.equal(rows2k, 39);
  assert.ok(rows2k <= 60, `expected bounded DOM rows, got ${rows2k}`);
  // Remaining height: (2000 - 39) * 24 = 47064px.
  assert.match(html2k, /aria-hidden="true" style="height:47064px"/);
  assert.match(html2k, /\/api\/v1\/orders\/0/);
  assert.doesNotMatch(html2k, /\/api\/v1\/orders\/1999/);

  const html5k = renderToStaticMarkup(React.createElement(LogsJsonView, { rows: makeLogs(5_000) }));
  const rows5k = (html5k.match(/role="listitem"/g) ?? []).length;
  assert.equal(rows5k, 39);
  assert.doesNotMatch(html5k, /\/api\/v1\/orders\/4999/);
});

test("buildStableLogRowKeys keeps keys stable when new rows prepend and disambiguates duplicate rows", () => {
  const initial = makeLogs(3);
  const initialKeys = buildStableLogRowKeys(initial);
  assert.equal(new Set(initialKeys).size, 3);

  // Prepend a new row and reassign positional `id`s (mimicking query refresh `${ms}-${index}`).
  const freshRow = makeLogs(1)[0];
  freshRow.raw = { ...freshRow.raw, _msg: "brand new log line", seq: 999 };
  freshRow.message = "brand new log line";
  const refreshed = [freshRow, ...initial].map((row, idx) => ({
    ...row,
    id: `${row.time}-${idx}`,
  }));
  const refreshedKeys = buildStableLogRowKeys(refreshed);

  // Existing rows shifted index from 0..2 -> 1..3, yet keep their exact stable keys.
  assert.deepEqual(refreshedKeys.slice(1), initialKeys);
  assert.notEqual(refreshedKeys[0], initialKeys[0]);

  // Duplicate identical rows in the same batch still get unique keys via occurrence counter.
  const duplicateBatch = [initial[0], { ...initial[0] }, { ...initial[0] }];
  const duplicateKeys = buildStableLogRowKeys(duplicateBatch);
  assert.equal(new Set(duplicateKeys).size, 3);
  assert.ok(duplicateKeys[0].endsWith("#0"));
  assert.ok(duplicateKeys[1].endsWith("#1"));
  assert.ok(duplicateKeys[2].endsWith("#2"));
});

test("LogsJsonView handles empty raw objects and duplicate row.id entries cleanly", () => {
  const emptyObjRows: LogRow[] = [0, 1, 2].map(() => ({
    id: "dup-id",
    time: 1_700_000_000_000,
    timeRaw: "2023-11-14T22:13:20.000Z",
    stream: '{service="api"}',
    message: "",
    level: "info",
    traceId: "",
    raw: {},
  }));
  const html = renderToStaticMarkup(React.createElement(LogsJsonView, { rows: emptyObjRows }));
  assert.equal((html.match(/role="listitem"/g) ?? []).length, 3);
});
