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
    equalLabels: editing ? Array.from(new Set(editing.equal_labels)) : [],
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

const RE2_INLINE_FLAGS = /^(?:[imsU]+(?:-[imsU]+)?|-[imsU]+)([:)])/;
const RE2_NAMED_GROUP = /^(?:P<|<)([A-Za-z0-9_]+)>/;
const RE2_POSIX_CLASS =
  /^\[:\^?(?:alnum|alpha|ascii|blank|cntrl|digit|graph|lower|print|punct|space|upper|word|xdigit):\]/;
const RE2_POSIX_CANDIDATE = /^\[:\^?[^:\]]*:\]/;
const RE2_UNICODE_CLASS = /^\\[pP](?:\{\^?[A-Za-z_ -]+\}|[A-Za-z])/;
const RE2_HEX_BRACE = /^\\x\{([0-9A-Fa-f]+)\}/;
const RE2_OCTAL_ESCAPE = /^\\([0-7]{2,3}|0)/;
const MAX_UNICODE_CODE_POINT = 0x10ffff;

function escapeJsRegexLiteral(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

function formatClampedUnicodeEscape(codePoint: number): string {
  const clamped = Math.min(codePoint, 0xffff);
  return `\\u${clamped.toString(16).padStart(4, "0")}`;
}

/**
 * Translates valid Go RE2 syntax constructs that JS `RegExp` does not natively
 * parse into syntactically equivalent JS regex tokens so client-side validation
 * aligns with Go `regexp.Compile`:
 * - `\Q...\E` quoted literals outside `[...]` (rejected inside `[...]`; lone `\E`/`\C` rejected)
 * - `(?P<name>...)` and `(?<name>...)` -> non-capturing `(?:...)` (RE2 allows digit-leading & duplicate names)
 * - inline RE2 flags `(?[imsU-]+)` -> `(?:)` and `(?[imsU-]+:...)` -> `(?:...)`; rejects JS lookarounds
 * - character-class leading `]` (`[]...]` / `[^]...]`), the 14 standard POSIX classes & `\p`/`\P` -> `\d`
 * - RE2 character-class escapes `\a`, `\x{...}` (<= 0x10ffff), and octal `\\([0-7]{2,3}|0)` -> JS escapes
 *
 * The Go backend remains the authoritative RE2 compiler and returns HTTP 400
 * for any remaining RE2-specific errors.
 */
export function normalizeRe2ForJsSyntaxCheck(pattern: string): string {
  let out = "";
  let inCharClass = false;
  let charClassStart = -1;
  let i = 0;

  while (i < pattern.length) {
    const ch = pattern[i];

    if (inCharClass) {
      if (ch === "\\" && i + 1 < pattern.length) {
        const next = pattern[i + 1];
        if (next === "Q" || next === "E" || next === "C") {
          throw new SyntaxError(`unsupported RE2 escape \\${next} inside character class`);
        }
        const rest = pattern.slice(i);
        const unicodeMatch = RE2_UNICODE_CLASS.exec(rest);
        if (unicodeMatch) {
          out += "\\d";
          i += unicodeMatch[0].length;
          continue;
        }
        if (next === "a") {
          out += "\\x07";
          i += 2;
          continue;
        }
        const hexMatch = RE2_HEX_BRACE.exec(rest);
        if (hexMatch) {
          const codePoint = Number.parseInt(hexMatch[1], 16);
          if (!Number.isFinite(codePoint) || codePoint > MAX_UNICODE_CODE_POINT) {
            throw new SyntaxError(`RE2 hex escape out of Unicode range: ${hexMatch[0]}`);
          }
          out += formatClampedUnicodeEscape(codePoint);
          i += hexMatch[0].length;
          continue;
        }
        const octalMatch = RE2_OCTAL_ESCAPE.exec(rest);
        if (octalMatch) {
          const codePoint = Number.parseInt(octalMatch[1], 8);
          out += formatClampedUnicodeEscape(codePoint);
          i += octalMatch[0].length;
          continue;
        }
        out += ch + next;
        i += 2;
        continue;
      }

      // Standard 14 POSIX character classes inside [...], e.g. [[:alnum:]-_]
      if (ch === "[" && pattern[i + 1] === ":") {
        const rest = pattern.slice(i);
        const posixMatch = RE2_POSIX_CLASS.exec(rest);
        if (posixMatch) {
          out += "\\d";
          i += posixMatch[0].length;
          continue;
        }
        if (RE2_POSIX_CANDIDATE.test(rest)) {
          throw new SyntaxError("unknown RE2 POSIX character class");
        }
      }

      // In RE2, `]` immediately after `[` or `[^` is a literal character, not the closing bracket.
      const isLeadingRightBracket =
        ch === "]" &&
        (i === charClassStart + 1 ||
          (i === charClassStart + 2 && pattern[charClassStart + 1] === "^"));
      if (isLeadingRightBracket) {
        out += "\\]";
        i += 1;
        continue;
      }
      if (ch === "]") {
        inCharClass = false;
        charClassStart = -1;
      }
      out += ch;
      i += 1;
      continue;
    }

    // Escape sequences outside character classes
    if (ch === "\\" && i + 1 < pattern.length) {
      const next = pattern[i + 1];
      if (next === "Q") {
        const endIdx = pattern.indexOf("\\E", i + 2);
        const literal = endIdx === -1 ? pattern.slice(i + 2) : pattern.slice(i + 2, endIdx);
        out += escapeJsRegexLiteral(literal);
        i = endIdx === -1 ? pattern.length : endIdx + 2;
        continue;
      }
      if (next === "E" || next === "C") {
        throw new SyntaxError(`unsupported RE2 escape \\${next}`);
      }
      const rest = pattern.slice(i);
      const unicodeMatch = RE2_UNICODE_CLASS.exec(rest);
      if (unicodeMatch) {
        out += "\\d";
        i += unicodeMatch[0].length;
        continue;
      }
      const hexMatch = RE2_HEX_BRACE.exec(rest);
      if (hexMatch) {
        const codePoint = Number.parseInt(hexMatch[1], 16);
        if (!Number.isFinite(codePoint) || codePoint > MAX_UNICODE_CODE_POINT) {
          throw new SyntaxError(`RE2 hex escape out of Unicode range: ${hexMatch[0]}`);
        }
        out += formatClampedUnicodeEscape(codePoint);
        i += hexMatch[0].length;
        continue;
      }
      out += ch + next;
      i += 2;
      continue;
    }

    if (ch === "[") {
      inCharClass = true;
      charClassStart = i;
      out += ch;
      i += 1;
      continue;
    }

    if (ch === "(" && pattern[i + 1] === "?") {
      const rest = pattern.slice(i + 2);
      if (rest.startsWith(":")) {
        out += "(?:";
        i += 3;
        continue;
      }
      const namedMatch = RE2_NAMED_GROUP.exec(rest);
      if (namedMatch) {
        out += "(?:";
        i += 2 + namedMatch[0].length;
        continue;
      }
      const flagsMatch = RE2_INLINE_FLAGS.exec(rest);
      if (flagsMatch) {
        const terminator = flagsMatch[1];
        out += terminator === ")" ? "(?:)" : "(?:";
        i += 2 + flagsMatch[0].length;
        continue;
      }
      throw new SyntaxError("unsupported RE2 group syntax");
    }

    out += ch;
    i += 1;
  }

  return out;
}

/**
 * Mirrors Go `matcher.RegexCache.Compile`: rejects patterns longer than 1024
 * UTF-8 bytes and compiles the RE2-normalized `^(?:<pattern>)$` to give
 * immediate syntax feedback without false-rejecting valid Go RE2 constructs.
 */
export function validateMatcherPattern(pattern: string): RegexValidationResult {
  if (utf8Encoder.encode(pattern).length > MAX_MATCHER_REGEX_BYTES) {
    return { ok: false, code: "regexTooLong" };
  }
  try {
    const normalized = normalizeRe2ForJsSyntaxCheck(pattern);
    // Same anchored envelope as Go's `regexp.Compile("^(?:" + pattern + ")$")`.
    new RegExp(`^(?:${normalized})$`);
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
