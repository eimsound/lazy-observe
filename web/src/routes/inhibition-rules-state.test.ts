import assert from "node:assert/strict";
import { test } from "vitest";

import type { InhibitionRule } from "../lib/api/alerts-contract.ts";
import { applyOptimisticData } from "../hooks/use-optimistic-list.ts";
import { ApiError } from "../lib/api/client.ts";
import { formatErrorMessage, isForbiddenError } from "../lib/api/errors.ts";
import { queryViewState } from "./query-view-state.ts";
import {
  MAX_INHIBITION_MATCHERS,
  MAX_MATCHER_REGEX_BYTES,
  addMatcherRow,
  commitEqualLabelDraft,
  createInhibitionFormDraft,
  partitionInhibitionRules,
  removeEqualLabel,
  removeMatcherRow,
  resolveInhibitionAccess,
  updateMatcherRow,
  validateInhibitionDraft,
  validateMatcherPattern,
} from "./inhibition-rules-state.ts";

const RULE_A: InhibitionRule = {
  id: "10",
  project_id: "1",
  source_matchers: [{ name: "alertname", op: "=", value: "HostDown" }],
  target_matchers: [{ name: "service", op: "=~", value: "api-.*" }],
  equal_labels: ["instance"],
  enabled: true,
  comment: "HostDown suppresses api services",
  created_at: "2026-11-21T00:00:00Z",
  updated_at: "2026-11-21T01:00:00Z",
};

const RULE_B: InhibitionRule = {
  id: "11",
  project_id: "1",
  source_matchers: [{ name: "severity", op: "=", value: "critical" }],
  target_matchers: [{ name: "severity", op: "=", value: "warning" }],
  equal_labels: ["alertname", "service"],
  enabled: false,
  comment: "Critical suppresses warning",
  created_at: "2026-11-21T00:00:00Z",
  updated_at: "2026-11-21T02:00:00Z",
};

test("inhibition rules five-state queryViewState covers pending, error, empty, stale, and ready", () => {
  assert.equal(
    queryViewState({ hasData: false, isLoading: true, isError: false, isEmpty: true }),
    "pending",
  );
  assert.equal(
    queryViewState({ hasData: false, isLoading: false, isError: true, isEmpty: true }),
    "error",
  );
  assert.equal(
    queryViewState({ hasData: true, isLoading: false, isError: false, isEmpty: true }),
    "empty",
  );
  assert.equal(
    queryViewState({ hasData: true, isLoading: false, isError: true, isEmpty: false }),
    "stale",
  );
  assert.equal(
    queryViewState({ hasData: true, isLoading: false, isError: false, isEmpty: false }),
    "ready",
  );
});

test("form draft cleanly initializes and resets without leaking state across create/edit switches", () => {
  const fresh = createInhibitionFormDraft(null);
  assert.deepEqual(fresh, {
    sourceMatchers: [{ name: "", op: "=", value: "" }],
    targetMatchers: [{ name: "", op: "=", value: "" }],
    equalLabels: [],
    equalLabelInput: "",
    enabled: true,
    comment: "",
  });

  const editA = createInhibitionFormDraft(RULE_A);
  assert.deepEqual(editA.sourceMatchers, RULE_A.source_matchers);
  assert.deepEqual(editA.targetMatchers, RULE_A.target_matchers);
  assert.deepEqual(editA.equalLabels, ["instance"]);
  assert.equal(editA.enabled, true);
  assert.equal(editA.comment, RULE_A.comment);

  // Mutating the draft must never mutate the source rule object.
  editA.sourceMatchers[0].value = "Mutated";
  editA.equalLabels.push("region");
  assert.equal(RULE_A.source_matchers[0].value, "HostDown");
  assert.deepEqual(RULE_A.equal_labels, ["instance"]);

  // Switching to RULE_B or back to create starts from a completely fresh draft.
  const editB = createInhibitionFormDraft(RULE_B);
  assert.equal(editB.enabled, false);
  assert.deepEqual(editB.equalLabels, ["alertname", "service"]);
  assert.equal(editB.equalLabelInput, "");

  const reopenedCreate = createInhibitionFormDraft(null);
  assert.equal(reopenedCreate.comment, "");
  assert.deepEqual(reopenedCreate.equalLabels, []);
  assert.equal(reopenedCreate.enabled, true);
});

test("dynamic matcher row helpers add, update, remove, and enforce bounds", () => {
  let rows = createInhibitionFormDraft(null).sourceMatchers;
  assert.equal(rows.length, 1);

  // Removing the last row keeps 1 empty row so inputs never disappear.
  assert.equal(removeMatcherRow(rows, 0).length, 1);

  rows = addMatcherRow(rows);
  assert.equal(rows.length, 2);

  rows = updateMatcherRow(rows, 0, { name: "alertname", value: "HostDown" });
  rows = updateMatcherRow(rows, 1, { name: "instance", op: "=~", value: "node-.*" });
  assert.deepEqual(rows, [
    { name: "alertname", op: "=", value: "HostDown" },
    { name: "instance", op: "=~", value: "node-.*" },
  ]);

  rows = removeMatcherRow(rows, 0);
  assert.deepEqual(rows, [{ name: "instance", op: "=~", value: "node-.*" }]);

  // Respects Go matcher.MaxMatchers (64) ceiling.
  let maxed = rows;
  for (let i = 0; i < MAX_INHIBITION_MATCHERS + 5; i += 1) {
    maxed = addMatcherRow(maxed);
  }
  assert.equal(maxed.length, MAX_INHIBITION_MATCHERS);
});

test("equal label helpers split commas/whitespace, deduplicate, and remove labels", () => {
  const first = commitEqualLabelDraft([], " instance, cluster  zone , instance ");
  assert.deepEqual(first, { labels: ["instance", "cluster", "zone"], remaining: "" });

  const second = commitEqualLabelDraft(first.labels, "service,cluster");
  assert.deepEqual(second, {
    labels: ["instance", "cluster", "zone", "service"],
    remaining: "",
  });

  assert.deepEqual(removeEqualLabel(second.labels, "cluster"), [
    "instance",
    "zone",
    "service",
  ]);
});

test("validateMatcherPattern mirrors Go anchored regex compilation and 1024-byte ceiling", () => {
  assert.deepEqual(validateMatcherPattern("api-(v1|v2)-.*"), { ok: true });

  const invalid = validateMatcherPattern("[unclosed");
  assert.equal(invalid.ok, false);
  if (!invalid.ok) {
    assert.equal(invalid.code, "regexInvalid");
  }

  const tooLong = validateMatcherPattern("a".repeat(MAX_MATCHER_REGEX_BYTES + 1));
  assert.deepEqual(tooLong, { ok: false, code: "regexTooLong" });

  // Multi-byte UTF-8 characters are measured in bytes, matching Go `len(pattern)`.
  const multiByteOver = validateMatcherPattern("中".repeat(342)); // 342 * 3 = 1026 bytes
  assert.deepEqual(multiByteOver, { ok: false, code: "regexTooLong" });
});

test("validateInhibitionDraft validates source/target matchers, regexes, and commits pending equal_labels", () => {
  const empty = createInhibitionFormDraft(null);
  assert.deepEqual(validateInhibitionDraft(empty), {
    ok: false,
    code: "sourceMatchersRequired",
  });

  // Incomplete source row (name without value)
  const incompleteSource = {
    ...empty,
    sourceMatchers: [{ name: "alertname", op: "=" as const, value: "  " }],
    targetMatchers: [{ name: "service", op: "=" as const, value: "checkout" }],
  };
  assert.deepEqual(validateInhibitionDraft(incompleteSource), {
    ok: false,
    code: "sourceMatcherIncomplete",
    params: { row: 1 },
  });

  // Missing target matchers
  const missingTarget = {
    ...empty,
    sourceMatchers: [{ name: "alertname", op: "=" as const, value: "HostDown" }],
  };
  assert.deepEqual(validateInhibitionDraft(missingTarget), {
    ok: false,
    code: "targetMatchersRequired",
  });

  // Invalid target regex
  const badRegex = {
    ...empty,
    sourceMatchers: [{ name: "alertname", op: "=" as const, value: "HostDown" }],
    targetMatchers: [{ name: "service", op: "=~" as const, value: "(broken" }],
  };
  const badRegexResult = validateInhibitionDraft(badRegex);
  assert.equal(badRegexResult.ok, false);
  if (!badRegexResult.ok) {
    assert.equal(badRegexResult.code, "targetRegexInvalid");
    assert.equal(badRegexResult.params?.row, 1);
  }

  // Valid draft ignores trailing blank rows, trims fields, and commits un-submitted equalLabelInput
  const valid = validateInhibitionDraft({
    sourceMatchers: [
      { name: " alertname ", op: "=", value: " HostDown " },
      { name: "", op: "=", value: "" },
    ],
    targetMatchers: [{ name: " service ", op: "=~", value: " api-.* " }],
    equalLabels: ["instance"],
    equalLabelInput: "cluster, instance",
    enabled: false,
    comment: "  host suppresses api  ",
  });
  assert.deepEqual(valid, {
    ok: true,
    input: {
      source_matchers: [{ name: "alertname", op: "=", value: "HostDown" }],
      target_matchers: [{ name: "service", op: "=~", value: "api-.*" }],
      equal_labels: ["instance", "cluster"],
      enabled: false,
      comment: "host suppresses api",
    },
  });
});

test("partitionInhibitionRules separates enabled and disabled rules in order", () => {
  const { enabled, disabled } = partitionInhibitionRules([RULE_A, RULE_B]);
  assert.deepEqual(enabled.map((r) => r.id), ["10"]);
  assert.deepEqual(disabled.map((r) => r.id), ["11"]);
});

test("RBAC access resolution enforces member+ read/write aligned with gateway /api/inhibition-rules", () => {
  assert.deepEqual(resolveInhibitionAccess("admin"), { canRead: true, canWrite: true });
  assert.deepEqual(resolveInhibitionAccess("member"), { canRead: true, canWrite: true });
  assert.deepEqual(resolveInhibitionAccess("viewer"), { canRead: false, canWrite: false });
  assert.deepEqual(resolveInhibitionAccess(null), { canRead: false, canWrite: false });
});

test("optimistic toggle update and error surfacing helpers preserve 403/404/500 messages", () => {
  const toggled = applyOptimisticData([RULE_A, RULE_B], (items) =>
    items.map((item) => (item.id === RULE_A.id ? { ...item, enabled: !item.enabled } : item)),
  );
  assert.equal(toggled?.[0].enabled, false);
  assert.equal(toggled?.[1].enabled, false);

  const forbidden = new ApiError("forbidden", 403);
  assert.equal(isForbiddenError(forbidden), true);
  assert.equal(formatErrorMessage(forbidden, "fallback"), "forbidden");

  const notFound = new ApiError("not found", 404);
  assert.equal(isForbiddenError(notFound), false);
  assert.equal(formatErrorMessage(notFound, "fallback"), "not found");

  const serverError = new ApiError("database unavailable", 500);
  assert.equal(formatErrorMessage(serverError, "fallback"), "database unavailable");
});
