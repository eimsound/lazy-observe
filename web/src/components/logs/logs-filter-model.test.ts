import assert from "node:assert/strict";
import { test } from "vitest";
import {
  appendSelectedValues,
  filterAndSortFields,
  getFieldFilterSummary,
  mergeFilterFields,
  resolveActiveField,
  sortFieldValues,
} from "./logs-filter-model";

test("mergeFilterFields appends missing stream filter fields with hits 0 and ignores non-stream filters", () => {
  const merged = mergeFilterFields(
    [
      { value: "app", hits: 100 },
      { value: "env", hits: 80 },
    ],
    [
      { field: "app", value: "api", operator: "=", isStream: true },
      { field: "pod", value: "pod-1", operator: "=", isStream: true },
      { field: "pod", value: "pod-2", operator: "!=", isStream: true },
      { field: "level", value: "error", operator: "=", isStream: false },
    ],
  );

  assert.deepEqual(merged, [
    { value: "app", hits: 100 },
    { value: "env", hits: 80 },
    { value: "pod", hits: 0 },
  ]);
});

test("filterAndSortFields filters case-insensitively and sorts by hits with tie-breaking", () => {
  const fields = [
    { value: "pod", hits: 10 },
    { value: "app", hits: 10 },
    { value: "Env", hits: 50 },
    { value: "host", hits: 30 },
  ];

  assert.deepEqual(
    filterAndSortFields(fields, "", "desc", "en").map((field) => field.value),
    ["Env", "host", "app", "pod"],
  );
  assert.deepEqual(
    filterAndSortFields(fields, "", "asc", "en").map((field) => field.value),
    ["app", "pod", "host", "Env"],
  );
  assert.deepEqual(
    filterAndSortFields(fields, "  env ", "desc", "en").map((field) => field.value),
    ["Env"],
  );
  assert.deepEqual(filterAndSortFields(fields, "missing", "desc", "en"), []);
});

test("appendSelectedValues and sortFieldValues include unknown selected values with hits 0 and ignore empty Any filters", () => {
  const values = [
    { value: "prod", hits: 50 },
    { value: "dev", hits: 50 },
    { value: "staging", hits: 20 },
  ];
  const filters = [
    { field: "env", value: "", operator: "!=" as const, isStream: true },
    { field: "env", value: "prod", operator: "=" as const, isStream: true },
    { field: "env", value: "canary", operator: "!=" as const, isStream: true },
    { field: "env", value: "alpha", operator: "=" as const, isStream: true },
  ];

  assert.deepEqual(appendSelectedValues(values, filters), [
    { value: "prod", hits: 50 },
    { value: "dev", hits: 50 },
    { value: "staging", hits: 20 },
    { value: "canary", hits: 0 },
    { value: "alpha", hits: 0 },
  ]);

  assert.deepEqual(
    sortFieldValues(values, filters, "desc", "en").map((item) => item.value),
    ["dev", "prod", "staging", "alpha", "canary"],
  );
  assert.deepEqual(
    sortFieldValues(values, filters, "asc", "en").map((item) => item.value),
    ["alpha", "canary", "staging", "dev", "prod"],
  );
});

test("getFieldFilterSummary computes none, any, all, and count badges accurately", () => {
  const fetched = [
    { value: "prod", hits: 50 },
    { value: "staging", hits: 20 },
  ];

  assert.equal(
    getFieldFilterSummary(
      "env",
      [
        { field: "env", value: "prod", operator: "=", isStream: false },
        { field: "app", value: "api", operator: "=", isStream: true },
      ],
      fetched,
    ).badgeKind,
    "none",
  );

  const anySummary = getFieldFilterSummary(
    "env",
    [
      { field: "env", value: "", operator: "!=", isStream: true },
      { field: "env", value: "prod", operator: "=", isStream: true },
    ],
    fetched,
  );
  assert.equal(anySummary.badgeKind, "any");
  assert.equal(anySummary.count, 2);
  assert.ok(anySummary.anyFilter);

  const allSummary = getFieldFilterSummary(
    "env",
    [
      { field: "env", value: "prod", operator: "=", isStream: true },
      { field: "env", value: "staging", operator: "!=", isStream: true },
    ],
    fetched,
  );
  assert.equal(allSummary.badgeKind, "all");
  assert.equal(allSummary.allListedValuesSelected, true);

  assert.equal(
    getFieldFilterSummary(
      "env",
      [{ field: "env", value: "prod", operator: "=", isStream: true }],
      undefined,
    ).badgeKind,
    "count",
  );
  assert.equal(
    getFieldFilterSummary(
      "env",
      [{ field: "env", value: "prod", operator: "=", isStream: true }],
      [],
    ).badgeKind,
    "count",
  );
});

test("resolveActiveField keeps visible selection and falls back to first visible field", () => {
  const visible = [
    { value: "app", hits: 10 },
    { value: "env", hits: 10 },
  ];

  assert.equal(resolveActiveField("env", visible), "env");
  assert.equal(resolveActiveField(null, visible), "app");
  assert.equal(resolveActiveField("pod", visible), "app");
  assert.equal(resolveActiveField("app", []), null);
});
