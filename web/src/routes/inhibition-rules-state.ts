import type { Role } from "@/lib/api/auth";
import type { InhibitionRule, InhibitionRuleInput, Matcher } from "@/lib/api/alerts-contract";
import { hasCapability } from "@/lib/auth/permissions";

/**
 * Pure state, validation, and access helpers for `/alerts/inhibitions`
 * (`internal/alert/matcher/matcher.go` + `internal/alert/service.go`).
 *
 * Kept free of React/Vite imports so Vitest exercises every validation and
 * state-reset edge case directly under Node.
 */

export const INHIBITION_MATCHER_OPS: readonly Matcher["op"][] = ["=", "=~"];
export const MAX_INHIBITION_MATCHERS = 64;
export const MAX_MATCHER_REGEX_BYTES = 1024;

export interface InhibitionFormDraft {
  sourceMatchers: Matcher[];
  targetMatchers: Matcher[];
  equalLabels: string[];
  equalLabelInput: string;
  enabled: boolean;
  comment: string;
}

export type RegexValidationResult =
  | { ok: true }
  | { ok: false; code: "regexTooLong" | "regexInvalid"; detail?: string };

export type InhibitionValidationErrorCode =
  | "sourceMatchersRequired"
  | "targetMatchersRequired"
  | "sourceMatcherIncomplete"
  | "targetMatcherIncomplete"
  | "sourceMatchersTooMany"
  | "targetMatchersTooMany"
  | "sourceRegexTooLong"
  | "targetRegexTooLong"
  | "sourceRegexInvalid"
  | "targetRegexInvalid";

export type InhibitionValidationResult =
  | { ok: true; input: InhibitionRuleInput }
  | {
      ok: false;
      code: InhibitionValidationErrorCode;
      params?: Record<string, string | number>;
    };

export function emptyMatcherRow(): Matcher {
  return { name: "", op: "=", value: "" };
}

function cloneMatchers(matchers: readonly Matcher[] | undefined): Matcher[] {
  if (!matchers || matchers.length === 0) return [emptyMatcherRow()];
  return matchers.map((m) => ({ name: m.name, op: m.op, value: m.value }));
}

/**
 * Builds a clean, independent form draft for create (`editing === null`) or
 * edit (`editing !== null`) so reopening or switching targets never leaks
 * previous form state.
 */
export function createInhibitionFormDraft(
  editing: InhibitionRule | null,
): InhibitionFormDraft {
  return {
    sourceMatchers: cloneMatchers(editing?.source_matchers),
    targetMatchers: cloneMatchers(editing?.target_matchers),
    equalLabels: editing ? [...editing.equal_labels] : [],
    equalLabelInput: "",
    enabled: editing?.enabled ?? true,
    comment: editing?.comment ?? "",
  };
}

export function addMatcherRow(rows: readonly Matcher[]): Matcher[] {
  if (rows.length >= MAX_INHIBITION_MATCHERS) return [...rows];
  return [...rows, emptyMatcherRow()];
}

export function updateMatcherRow(
  rows: readonly Matcher[],
  index: number,
  patch: Partial<Matcher>,
): Matcher[] {
  return rows.map((row, idx) => (idx === index ? { ...row, ...patch } : row));
}

export function removeMatcherRow(rows: readonly Matcher[], index: number): Matcher[] {
  if (rows.length <= 1) return [...rows];
  return rows.filter((_, idx) => idx !== index);
}

/**
 * Splits a raw equal-label input string on commas or whitespace, trims every
 * token, drops blanks, and merges new unique labels while preserving order.
 */
export function commitEqualLabelDraft(
  existing: readonly string[],
  rawInput: string,
): { labels: string[]; remaining: string } {
  const seen = new Set(existing);
  const next = [...existing];
  const tokens = rawInput.split(/[\s,]+/);
  for (const token of tokens) {
    const trimmed = token.trim();
    if (!trimmed || seen.has(trimmed)) continue;
    seen.add(trimmed);
    next.push(trimmed);
  }
  return { labels: next, remaining: "" };
}

export function removeEqualLabel(existing: readonly string[], label: string): string[] {
  return existing.filter((item) => item !== label);
}

const utf8Encoder = new TextEncoder();

/**
 * Mirrors Go `matcher.RegexCache.Compile`: rejects patterns longer than 1024
 * UTF-8 bytes and compiles `^(?:<pattern>)$` to give immediate syntax feedback.
 */
export function validateMatcherPattern(pattern: string): RegexValidationResult {
  if (utf8Encoder.encode(pattern).length > MAX_MATCHER_REGEX_BYTES) {
    return { ok: false, code: "regexTooLong" };
  }
  try {
    // Same anchored envelope as Go's `regexp.Compile("^(?:" + pattern + ")$")`.
    new RegExp(`^(?:${pattern})$`);
    return { ok: true };
  } catch (err) {
    return {
      ok: false,
      code: "regexInvalid",
      detail: err instanceof Error ? err.message : undefined,
    };
  }
}

function validateMatcherSection(
  rows: readonly Matcher[],
  side: "source" | "target",
):
  | { ok: true; matchers: Matcher[] }
  | { ok: false; code: InhibitionValidationErrorCode; params?: Record<string, string | number> } {
  const cleaned: Matcher[] = [];
  for (let i = 0; i < rows.length; i += 1) {
    const name = rows[i].name.trim();
    const rawValue = rows[i].value;
    const trimmedValue = rawValue.trim();
    if (!name && !trimmedValue) continue;
    if (!name || !trimmedValue) {
      return {
        ok: false,
        code: side === "source" ? "sourceMatcherIncomplete" : "targetMatcherIncomplete",
        params: { row: i + 1 },
      };
    }
    const op: Matcher["op"] = rows[i].op === "=~" ? "=~" : "=";
    if (op === "=~") {
      const regexCheck = validateMatcherPattern(trimmedValue);
      if (!regexCheck.ok) {
        return {
          ok: false,
          code:
            regexCheck.code === "regexTooLong"
              ? side === "source"
                ? "sourceRegexTooLong"
                : "targetRegexTooLong"
              : side === "source"
                ? "sourceRegexInvalid"
                : "targetRegexInvalid",
          params: {
            row: i + 1,
            max: MAX_MATCHER_REGEX_BYTES,
            ...(regexCheck.detail ? { detail: regexCheck.detail } : {}),
          },
        };
      }
    }
    cleaned.push({ name, op, value: trimmedValue });
  }

  if (cleaned.length === 0) {
    return {
      ok: false,
      code: side === "source" ? "sourceMatchersRequired" : "targetMatchersRequired",
    };
  }
  if (cleaned.length > MAX_INHIBITION_MATCHERS) {
    return {
      ok: false,
      code: side === "source" ? "sourceMatchersTooMany" : "targetMatchersTooMany",
      params: { max: MAX_INHIBITION_MATCHERS },
    };
  }

  return { ok: true, matchers: cleaned };
}

/**
 * Validates the form draft against backend `CompileInhibitionRule` invariants
 * and produces a clean `InhibitionRuleInput` wire payload. Any pending text in
 * `equalLabelInput` is automatically committed so users never lose an
 * un-confirmed label tag on submit.
 */
export function validateInhibitionDraft(
  draft: InhibitionFormDraft,
): InhibitionValidationResult {
  const source = validateMatcherSection(draft.sourceMatchers, "source");
  if (!source.ok) return source;

  const target = validateMatcherSection(draft.targetMatchers, "target");
  if (!target.ok) return target;

  const { labels: equalLabels } = commitEqualLabelDraft(
    draft.equalLabels,
    draft.equalLabelInput,
  );

  return {
    ok: true,
    input: {
      source_matchers: source.matchers,
      target_matchers: target.matchers,
      equal_labels: equalLabels,
      enabled: draft.enabled,
      comment: draft.comment.trim(),
    },
  };
}

export function partitionInhibitionRules(rules: readonly InhibitionRule[]): {
  enabled: InhibitionRule[];
  disabled: InhibitionRule[];
} {
  const enabled: InhibitionRule[] = [];
  const disabled: InhibitionRule[] = [];
  for (const rule of rules) {
    if (rule.enabled) {
      enabled.push(rule);
    } else {
      disabled.push(rule);
    }
  }
  return { enabled, disabled };
}

/**
 * Central RBAC access resolution for `/alerts/inhibitions`, matching
 * `internal/gateway/gateway.go` (`roleMember` for GET/POST/PATCH/PUT/DELETE
 * `/api/inhibition-rules`).
 */
export function resolveInhibitionAccess(role: Role | null): {
  canRead: boolean;
  canWrite: boolean;
} {
  return {
    canRead: hasCapability(role, "alerts:read"),
    canWrite: hasCapability(role, "alerts:write"),
  };
}
