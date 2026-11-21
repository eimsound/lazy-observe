import assert from "node:assert/strict";
import { test } from "vitest";
import {
  parseLogsPresentationState,
  queryColumnWidths,
  writeLogsPresentationParams,
} from "./logs-presentation-state";

test("queryColumnWidths validates JSON objects, keys, and bounds", () => {
  assert.deepEqual(queryColumnWidths(null), {});
  assert.deepEqual(queryColumnWidths("not-json"), {});
  assert.deepEqual(queryColumnWidths("[1,2,3]"), {});
  assert.deepEqual(
    queryColumnWidths(JSON.stringify({ _msg: 1200, service: 40, level: "220", bad: "x" })),
    { _msg: 800, service: 96, level: 220 },
  );
});

test("presentation state parses and writes URL params while preserving workspace params", () => {
  const initialHref = "/logs?q=service:api&run=1&autocomplete=1&hide_logs=1&graph=stats"
    + "&graph_by=service&graph_top=12&graph_step=30s&stacked=1&cumulative=1&hide_chart=1"
    + "&group_by=level&display_fields=service,env&single_line=0&compact_groups=1"
    + "&markdown=1&ansi=1&disable_hover=1&table_fields=_time,service&rows_per_page=250"
    + "&table_widths=%7B%22service%22%3A280%7D";

  const parsed = parseLogsPresentationState(initialHref);
  assert.equal(parsed.autocomplete, false);
  assert.equal(parsed.hideLogs, true);
  assert.deepEqual(parsed.chart, {
    graphMode: "stats",
    groupBy: "service",
    topN: 12,
    step: "30s",
    stacked: true,
    cumulative: true,
    hidden: true,
  });
  assert.equal(parsed.groupSettings.groupBy, "level");
  assert.deepEqual(parsed.groupSettings.displayFields, ["service", "env"]);
  assert.equal(parsed.groupSettings.singleLine, false);
  assert.equal(parsed.groupSettings.compactHeader, true);
  assert.equal(parsed.groupSettings.markdownParsing, true);
  assert.equal(parsed.groupSettings.ansiParsing, true);
  assert.equal(parsed.groupSettings.disableHover, true);
  assert.equal(parsed.groupSettings.pageSize, 250);
  assert.deepEqual(parsed.tableSettings.visibleFields, ["_time", "service"]);
  assert.deepEqual(parsed.tableSettings.columnWidths, { service: 280 });
  assert.equal(parsed.tableSettings.pageSize, 250);

  const rewritten = writeLogsPresentationParams("/logs?q=service:api&run=1", {
    autocomplete: parsed.autocomplete,
    hideLogs: parsed.hideLogs,
    chart: parsed.chart,
    groupSettings: parsed.groupSettings,
    tableSettings: parsed.tableSettings,
    view: "table",
  });
  const url = new URL(rewritten, "https://lazy-observe.test");
  assert.equal(url.searchParams.get("q"), "service:api");
  assert.equal(url.searchParams.get("run"), "1");
  assert.equal(url.searchParams.get("autocomplete"), "1");
  assert.equal(url.searchParams.get("graph"), "stats");
  assert.equal(url.searchParams.get("table_fields"), "_time,service");
  assert.equal(url.searchParams.get("rows_per_page"), "250");
  assert.equal(url.searchParams.get("single_line"), "0");
});

test("writeLogsPresentationParams omits defaults and leaves live rows_per_page untouched", () => {
  const defaults = parseLogsPresentationState("/logs");
  const written = writeLogsPresentationParams("/logs?view=live&rows_per_page=500", {
    autocomplete: defaults.autocomplete,
    hideLogs: defaults.hideLogs,
    chart: defaults.chart,
    groupSettings: defaults.groupSettings,
    tableSettings: defaults.tableSettings,
    view: "live",
  });
  const url = new URL(written, "https://lazy-observe.test");
  assert.equal(url.searchParams.get("rows_per_page"), "500");
  assert.equal(url.searchParams.has("table_fields"), false);
  assert.equal(url.searchParams.has("single_line"), false);
  assert.equal(url.searchParams.has("graph"), false);
});
