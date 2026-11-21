/**
 * Pure PromQL visual query builder model, generator, escaper, recursive-descent
 * reverse parser, and bidirectional CodeMirror sync controller.
 *
 * Kept free of React/DOM dependencies so every edge case runs directly under
 * Vitest in the node environment.
 */

export const PROMQL_LABEL_OPERATORS = ["=", "!=", "=~", "!~"] as const;
export type PromQLLabelOperator = (typeof PROMQL_LABEL_OPERATORS)[number];

export interface PromQLLabelFilter {
  id: string;
  label: string;
  operator: PromQLLabelOperator;
  value: string;
}

export const PROMQL_RANGE_FUNCTIONS = [
  "rate",
  "irate",
  "increase",
  "delta",
  "avg_over_time",
  "max_over_time",
  "min_over_time",
  "sum_over_time",
  "count_over_time",
  "last_over_time",
] as const;

export type PromQLRangeFunctionName = (typeof PROMQL_RANGE_FUNCTIONS)[number];
export type PromQLRangeFunction = "none" | PromQLRangeFunctionName;

export const PROMQL_RANGE_INTERVAL_PRESETS = [
  "$__rate_interval",
  "1m",
  "5m",
  "15m",
  "30m",
  "1h",
  "6h",
  "24h",
] as const;

export const DEFAULT_PROMQL_RANGE_INTERVAL = "5m";

export const PROMQL_AGGREGATIONS = [
  "sum",
  "avg",
  "max",
  "min",
  "count",
  "topk",
  "bottomk",
] as const;

export type PromQLAggregationName = (typeof PROMQL_AGGREGATIONS)[number];
export type PromQLAggregation = "none" | PromQLAggregationName;
export type PromQLAggregationMode = "none" | "by" | "without";

export const PROMQL_QUANTILE_PRESETS = ["0.5", "0.9", "0.95", "0.99"] as const;

export interface PromQLBuilderState {
  metric: string;
  filters: PromQLLabelFilter[];
  rangeFunction: PromQLRangeFunction;
  rangeInterval: string;
  aggregation: PromQLAggregation;
  aggregationMode: PromQLAggregationMode;
  aggregationLabels: string[];
  aggregationParam: string;
  histogramQuantile: string;
}

export type PromQLParseFailureReason = "complex" | "syntax";

export type PromQLParseResult =
  | { ok: true; state: PromQLBuilderState }
  | { ok: false; reason: PromQLParseFailureReason };

export type PromQLBuilderStatus = "synced" | PromQLParseFailureReason;

export interface PromQLBuilderControllerState {
  draft: PromQLBuilderState;
  lastSyncedQuery: string;
  status: PromQLBuilderStatus;
}

export type UnescapeResult = { ok: true; value: string } | { ok: false };

const RANGE_FUNCTION_SET = new Set<string>(PROMQL_RANGE_FUNCTIONS);
const AGGREGATION_SET = new Set<string>(PROMQL_AGGREGATIONS);
const PARAM_AGGREGATION_SET = new Set<string>(["topk", "bottomk"]);
const SIMPLE_IDENT_PATTERN = /^[a-zA-Z_:][a-zA-Z0-9_.:]*$/;
const RANGE_INTERVAL_PATTERN = /^(?:(?:\d+(?:\.\d+)?(?:ms|[smhdwy]))+|\$__[a-zA-Z0-9_]+)$/;
const QUANTILE_PATTERN = /^(?:0(?:\.\d+)?|1(?:\.0+)?|\.\d+)$/;

let filterIdCounter = 1;

export function nextBuilderFilterId(): string {
  return `filter-${filterIdCounter++}`;
}

export function createEmptyBuilderState(): PromQLBuilderState {
  return {
    metric: "",
    filters: [],
    rangeFunction: "none",
    rangeInterval: DEFAULT_PROMQL_RANGE_INTERVAL,
    aggregation: "none",
    aggregationMode: "none",
    aggregationLabels: [],
    aggregationParam: "5",
    histogramQuantile: "",
  };
}

export function escapePromQLLabelValue(value: string): string {
  let out = "";
  for (let i = 0; i < value.length; i += 1) {
    const ch = value[i];
    const code = value.charCodeAt(i);
    if (ch === "\\") out += "\\\\";
    else if (ch === '"') out += '\\"';
    else if (ch === "\n") out += "\\n";
    else if (ch === "\r") out += "\\r";
    else if (ch === "\t") out += "\\t";
    else if (code <= 0x1f) out += `\\x${code.toString(16).padStart(2, "0")}`;
    else out += ch;
  }
  return out;
}

/**
 * Format a label name for PromQL matchers and `by`/`without` lists. Bare
 * identifier labels stay unquoted; UTF-8 / punctuation labels (such as
 * `k8s-app` or `app/name`) are quoted and escaped per PromQL UTF-8 rules.
 */
export function formatPromQLLabel(label: string): string {
  return SIMPLE_IDENT_PATTERN.test(label) ? label : `"${escapePromQLLabelValue(label)}"`;
}

function isHexDigit(ch: string): boolean {
  return (
    (ch >= "0" && ch <= "9")
    || (ch >= "a" && ch <= "f")
    || (ch >= "A" && ch <= "F")
  );
}

function isOctalDigit(ch: string): boolean {
  return ch >= "0" && ch <= "7";
}

function readExactHex(raw: string, start: number, count: number): number | null {
  if (start + count > raw.length) return null;
  for (let i = 0; i < count; i += 1) {
    if (!isHexDigit(raw[start + i])) return null;
  }
  return Number.parseInt(raw.slice(start, start + count), 16);
}

/**
 * Strictly unescape a PromQL double- or single-quoted string literal per the
 * PromQL specification (`\a`, `\b`, `\f`, `\n`, `\r`, `\t`, `\v`, `\\`, matching
 * quote delimiter, `\[0-7]{3}`, `\xHH`, `\uHHHH`, `\UHHHHHHHH`). Unknown or
 * malformed escape sequences return `{ ok: false }`.
 */
export function unescapePromQLLabelValue(
  raw: string,
  quote: '"' | "'" = '"',
): UnescapeResult {
  let out = "";
  for (let i = 0; i < raw.length; i += 1) {
    const ch = raw[i];
    if (ch !== "\\") {
      out += ch;
      continue;
    }
    if (i + 1 >= raw.length) return { ok: false };
    const next = raw[i + 1];
    i += 1;

    if (next === "\\") {
      out += "\\";
    } else if (next === quote) {
      out += quote;
    } else if (next === "a") {
      out += "\x07";
    } else if (next === "b") {
      out += "\b";
    } else if (next === "f") {
      out += "\f";
    } else if (next === "n") {
      out += "\n";
    } else if (next === "r") {
      out += "\r";
    } else if (next === "t") {
      out += "\t";
    } else if (next === "v") {
      out += "\v";
    } else if (next === "x") {
      const value = readExactHex(raw, i + 1, 2);
      if (value === null) return { ok: false };
      out += String.fromCharCode(value);
      i += 2;
    } else if (next === "u") {
      const value = readExactHex(raw, i + 1, 4);
      if (value === null || (value >= 0xd800 && value <= 0xdfff)) return { ok: false };
      out += String.fromCodePoint(value);
      i += 4;
    } else if (next === "U") {
      const value = readExactHex(raw, i + 1, 8);
      if (value === null || value > 0x10ffff || (value >= 0xd800 && value <= 0xdfff)) {
        return { ok: false };
      }
      out += String.fromCodePoint(value);
      i += 8;
    } else if (isOctalDigit(next)) {
      if (i + 2 >= raw.length || !isOctalDigit(raw[i + 1]) || !isOctalDigit(raw[i + 2])) {
        return { ok: false };
      }
      const value = Number.parseInt(raw.slice(i, i + 3), 8);
      if (value > 0xff) return { ok: false };
      out += String.fromCharCode(value);
      i += 2;
    } else {
      return { ok: false };
    }
  }
  return { ok: true, value: out };
}

export function isValidPromQLRangeInterval(value: string): boolean {
  return RANGE_INTERVAL_PATTERN.test(value.trim());
}

export function isValidPromQLQuantile(value: string): boolean {
  const trimmed = value.trim();
  if (!QUANTILE_PATTERN.test(trimmed)) return false;
  const num = Number(trimmed);
  return Number.isFinite(num) && num >= 0 && num <= 1;
}

function isValidRegexPattern(pattern: string): boolean {
  // Normalize common Go/RE2 inline flag groups like `(?i)` / `(?i:...)` so valid
  // PromQL regex matchers are accepted while unclosed brackets/parens are skipped.
  const normalized = pattern
    .replace(/\(\?[imsU-]+:/g, "(?:")
    .replace(/\(\?[imsU-]+\)/g, "");
  try {
    new RegExp(normalized);
    return true;
  } catch {
    return false;
  }
}

function normalizePositiveIntParam(raw: string, fallback = "5"): string {
  const trimmed = raw.trim();
  if (!/^\d+$/.test(trimmed)) return fallback;
  const parsed = Number(trimmed);
  return Number.isSafeInteger(parsed) && parsed >= 1 ? String(parsed) : fallback;
}

function formatMatcher(label: string, operator: PromQLLabelOperator, value: string): string {
  return `${formatPromQLLabel(label)}${operator}"${escapePromQLLabelValue(value)}"`;
}

/**
 * Build a PromQL series selector (`match[]`) used for querying `/api/v1/labels`
 * and `/api/v1/label/:name/values` scoped to the current metric and sibling
 * label filters. In-progress invalid regex filters are skipped so metadata
 * requests do not return transient 400 errors while the user is typing.
 */
export function buildMetadataMatchSelector(
  state: Pick<PromQLBuilderState, "metric" | "filters">,
  options?: { excludeFilterId?: string },
): string | undefined {
  const parts: string[] = [];
  const metric = state.metric.trim();
  if (metric) {
    parts.push(formatMatcher("__name__", "=", metric));
  }
  for (const filter of state.filters) {
    if (options?.excludeFilterId && filter.id === options.excludeFilterId) continue;
    const label = filter.label.trim();
    if (!label) continue;
    if (
      (filter.operator === "=~" || filter.operator === "!~")
      && !isValidRegexPattern(filter.value)
    ) {
      continue;
    }
    parts.push(formatMatcher(label, filter.operator, filter.value));
  }
  return parts.length > 0 ? `{${parts.join(",")}}` : undefined;
}

/**
 * Serialize a structured builder state into a valid PromQL / MetricsQL string.
 * Incomplete filter rows (blank label name) are ignored so adding a new row
 * never emits broken PromQL.
 */
export function buildPromQL(state: PromQLBuilderState): string {
  const metric = state.metric.trim();
  const validFilters = state.filters
    .map((filter) => ({
      label: filter.label.trim(),
      operator: filter.operator,
      value: filter.value,
    }))
    .filter((filter) => filter.label.length > 0);

  if (!metric && validFilters.length === 0) return "";

  let expr: string;
  if (!metric) {
    expr = `{${validFilters.map((f) => formatMatcher(f.label, f.operator, f.value)).join(", ")}}`;
  } else if (SIMPLE_IDENT_PATTERN.test(metric)) {
    expr = validFilters.length > 0
      ? `${metric}{${validFilters.map((f) => formatMatcher(f.label, f.operator, f.value)).join(", ")}}`
      : metric;
  } else {
    const matchers = [
      formatMatcher("__name__", "=", metric),
      ...validFilters.map((f) => formatMatcher(f.label, f.operator, f.value)),
    ];
    expr = `{${matchers.join(", ")}}`;
  }

  if (state.rangeFunction !== "none") {
    const rawInterval = state.rangeInterval.trim();
    const interval = isValidPromQLRangeInterval(rawInterval)
      ? rawInterval
      : DEFAULT_PROMQL_RANGE_INTERVAL;
    expr = `${state.rangeFunction}(${expr}[${interval}])`;
  }

  if (state.aggregation !== "none") {
    const labels = [...new Set(state.aggregationLabels.map((label) => label.trim()).filter(Boolean))];
    const hasClause =
      (state.aggregationMode === "by" || state.aggregationMode === "without") && labels.length > 0;
    const clause = hasClause
      ? ` ${state.aggregationMode} (${labels.map(formatPromQLLabel).join(", ")})`
      : "";
    if (PARAM_AGGREGATION_SET.has(state.aggregation)) {
      const k = normalizePositiveIntParam(state.aggregationParam);
      expr = hasClause
        ? `${state.aggregation}${clause} (${k}, ${expr})`
        : `${state.aggregation}(${k}, ${expr})`;
    } else {
      expr = hasClause
        ? `${state.aggregation}${clause} (${expr})`
        : `${state.aggregation}(${expr})`;
    }
  }

  const quantile = state.histogramQuantile.trim();
  if (quantile && isValidPromQLQuantile(quantile)) {
    expr = `histogram_quantile(${quantile}, ${expr})`;
  }

  return expr;
}

// ---------------------------------------------------------------------------
// Recursive-descent tokenizer & parser for reverse-syncing Code -> Builder
// ---------------------------------------------------------------------------

type TokenKind =
  | "ident"
  | "number"
  | "string"
  | "range"
  | "op"
  | "lparen"
  | "rparen"
  | "lbrace"
  | "rbrace"
  | "comma"
  | "comment"
  | "other"
  | "eof";

interface Token {
  kind: TokenKind;
  value: string;
  quote?: '"' | "'" | "`";
}

type TokenizeResult =
  | { ok: true; tokens: Token[] }
  | { ok: false; reason: PromQLParseFailureReason };

function isIdentStart(ch: string): boolean {
  return (ch >= "a" && ch <= "z") || (ch >= "A" && ch <= "Z") || ch === "_" || ch === ":";
}

function isIdentPart(ch: string): boolean {
  return isIdentStart(ch) || (ch >= "0" && ch <= "9") || ch === ".";
}

function isDigit(ch: string): boolean {
  return ch >= "0" && ch <= "9";
}

function tokenizePromQL(input: string): TokenizeResult {
  const tokens: Token[] = [];
  let i = 0;

  while (i < input.length) {
    const ch = input[i];
    if (ch === " " || ch === "\t" || ch === "\n" || ch === "\r") {
      i += 1;
      continue;
    }

    if (ch === "#") {
      tokens.push({ kind: "comment", value: input.slice(i) });
      break;
    }

    if (ch === "(") {
      tokens.push({ kind: "lparen", value: ch });
      i += 1;
      continue;
    }
    if (ch === ")") {
      tokens.push({ kind: "rparen", value: ch });
      i += 1;
      continue;
    }
    if (ch === "{") {
      tokens.push({ kind: "lbrace", value: ch });
      i += 1;
      continue;
    }
    if (ch === "}") {
      tokens.push({ kind: "rbrace", value: ch });
      i += 1;
      continue;
    }
    if (ch === ",") {
      tokens.push({ kind: "comma", value: ch });
      i += 1;
      continue;
    }

    if (ch === "=" || ch === "!") {
      const next = input[i + 1];
      if (ch === "=" && next === "~") {
        tokens.push({ kind: "op", value: "=~" });
        i += 2;
        continue;
      }
      if (ch === "!" && (next === "=" || next === "~")) {
        tokens.push({ kind: "op", value: `${ch}${next}` });
        i += 2;
        continue;
      }
      if (ch === "=" && next === "=") {
        tokens.push({ kind: "other", value: "==" });
        i += 2;
        continue;
      }
      if (ch === "=") {
        tokens.push({ kind: "op", value: "=" });
        i += 1;
        continue;
      }
      tokens.push({ kind: "other", value: ch });
      i += 1;
      continue;
    }

    if (ch === "[") {
      const closeIdx = input.indexOf("]", i + 1);
      if (closeIdx === -1) return { ok: false, reason: "syntax" };
      const inner = input.slice(i + 1, closeIdx).trim();
      if (!inner) return { ok: false, reason: "syntax" };
      tokens.push({ kind: "range", value: inner });
      i = closeIdx + 1;
      continue;
    }

    if (ch === '"' || ch === "'") {
      const quote = ch;
      let raw = "";
      let closed = false;
      i += 1;
      while (i < input.length) {
        const cur = input[i];
        if (cur === "\\") {
          if (i + 1 >= input.length) return { ok: false, reason: "syntax" };
          raw += cur + input[i + 1];
          i += 2;
          continue;
        }
        if (cur === quote) {
          closed = true;
          i += 1;
          break;
        }
        raw += cur;
        i += 1;
      }
      if (!closed) return { ok: false, reason: "syntax" };
      const decoded = unescapePromQLLabelValue(raw, quote);
      if (!decoded.ok) return { ok: false, reason: "syntax" };
      tokens.push({ kind: "string", value: decoded.value, quote });
      continue;
    }

    if (ch === "`") {
      const closeIdx = input.indexOf("`", i + 1);
      if (closeIdx === -1) return { ok: false, reason: "syntax" };
      tokens.push({ kind: "string", value: input.slice(i + 1, closeIdx), quote: "`" });
      i = closeIdx + 1;
      continue;
    }

    if (isDigit(ch) || (ch === "." && i + 1 < input.length && isDigit(input[i + 1]))) {
      const start = i;
      let seenDot = ch === ".";
      i += 1;
      while (i < input.length) {
        const cur = input[i];
        if (isDigit(cur)) {
          i += 1;
        } else if (cur === "." && !seenDot) {
          seenDot = true;
          i += 1;
        } else {
          break;
        }
      }
      tokens.push({ kind: "number", value: input.slice(start, i) });
      continue;
    }

    if (isIdentStart(ch)) {
      const start = i;
      i += 1;
      while (i < input.length && isIdentPart(input[i])) {
        i += 1;
      }
      tokens.push({ kind: "ident", value: input.slice(start, i) });
      continue;
    }

    tokens.push({ kind: "other", value: ch });
    i += 1;
  }

  tokens.push({ kind: "eof", value: "" });
  return { ok: true, tokens };
}

class BuilderParser {
  private readonly tokens: Token[];
  private pos = 0;

  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  private peek(offset = 0): Token {
    return this.tokens[Math.min(this.pos + offset, this.tokens.length - 1)];
  }

  private next(): Token {
    const token = this.peek();
    if (this.pos < this.tokens.length - 1) this.pos += 1;
    return token;
  }

  parse(): PromQLParseResult {
    if (this.tokens.some((token) => token.kind === "comment" || token.kind === "other")) {
      return { ok: false, reason: "complex" };
    }

    const state = createEmptyBuilderState();
    const res = this.parseTopLevel(state);
    if (!res.ok) return res;

    if (this.peek().kind !== "eof") {
      // Unconsumed trailing tokens such as `offset 5m` or binary keywords (`and`/`or`).
      return { ok: false, reason: "complex" };
    }
    return { ok: true, state };
  }

  private parseTopLevel(state: PromQLBuilderState): { ok: true } | { ok: false; reason: PromQLParseFailureReason } {
    const tok = this.peek();
    if (tok.kind === "ident" && tok.value === "histogram_quantile" && this.peek(1).kind === "lparen") {
      return this.parseHistogramQuantile(state);
    }
    return this.parseAggregationOrBelow(state);
  }

  private parseHistogramQuantile(
    state: PromQLBuilderState,
  ): { ok: true } | { ok: false; reason: PromQLParseFailureReason } {
    this.next(); // histogram_quantile
    this.next(); // (
    const qTok = this.peek();
    if (qTok.kind !== "number") {
      return { ok: false, reason: qTok.kind === "eof" || qTok.kind === "rparen" ? "syntax" : "complex" };
    }
    if (!isValidPromQLQuantile(qTok.value)) {
      return { ok: false, reason: "complex" };
    }
    this.next();
    if (this.peek().kind !== "comma") {
      return { ok: false, reason: "syntax" };
    }
    this.next(); // ,
    state.histogramQuantile = qTok.value;

    const inner = this.parseAggregationOrBelow(state);
    if (!inner.ok) return inner;

    if (this.peek().kind !== "rparen") {
      return { ok: false, reason: this.peek().kind === "eof" ? "syntax" : "complex" };
    }
    this.next(); // )
    return { ok: true };
  }

  private parseAggregationOrBelow(
    state: PromQLBuilderState,
  ): { ok: true } | { ok: false; reason: PromQLParseFailureReason } {
    const tok = this.peek();
    const lower = tok.kind === "ident" ? tok.value.toLowerCase() : "";
    if (tok.kind === "ident" && AGGREGATION_SET.has(lower)) {
      const nextTok = this.peek(1);
      const nextLower = nextTok.kind === "ident" ? nextTok.value.toLowerCase() : "";
      if (nextTok.kind === "lparen" || nextLower === "by" || nextLower === "without") {
        return this.parseAggregation(state, lower as PromQLAggregationName);
      }
    }
    return this.parseRangeOrSelector(state);
  }

  private parseGroupingClause():
    | { ok: true; mode: "by" | "without"; labels: string[] }
    | { ok: false; reason: PromQLParseFailureReason } {
    const kw = this.next();
    const mode = kw.value.toLowerCase() as "by" | "without";
    if (this.peek().kind !== "lparen") return { ok: false, reason: "syntax" };
    this.next(); // (
    const labels: string[] = [];
    while (this.peek().kind !== "rparen") {
      const cur = this.peek();
      if (cur.kind === "eof") return { ok: false, reason: "syntax" };
      if (cur.kind === "ident") {
        labels.push(this.next().value);
      } else if (cur.kind === "string" && cur.quote === '"') {
        if (cur.value.length === 0) return { ok: false, reason: "syntax" };
        labels.push(this.next().value);
      } else {
        return { ok: false, reason: cur.kind === "string" ? "syntax" : "complex" };
      }
      if (this.peek().kind === "comma") {
        this.next();
      } else if (this.peek().kind !== "rparen") {
        return { ok: false, reason: "syntax" };
      }
    }
    this.next(); // )
    if (labels.length === 0) {
      // Preserve explicit `by ()` / `without ()` semantics in code mode.
      return { ok: false, reason: "complex" };
    }
    return { ok: true, mode, labels };
  }

  private parseAggregation(
    state: PromQLBuilderState,
    agg: PromQLAggregationName,
  ): { ok: true } | { ok: false; reason: PromQLParseFailureReason } {
    this.next(); // aggregation operator
    state.aggregation = agg;

    let hasLeadingClause = false;
    const maybeLeading = this.peek();
    if (
      maybeLeading.kind === "ident"
      && (maybeLeading.value.toLowerCase() === "by" || maybeLeading.value.toLowerCase() === "without")
    ) {
      const clause = this.parseGroupingClause();
      if (!clause.ok) return clause;
      state.aggregationMode = clause.mode;
      state.aggregationLabels = clause.labels;
      hasLeadingClause = true;
    }

    if (this.peek().kind !== "lparen") return { ok: false, reason: "syntax" };
    this.next(); // (

    if (this.peek().kind === "rparen") return { ok: false, reason: "syntax" };

    if (PARAM_AGGREGATION_SET.has(agg)) {
      const kTok = this.peek();
      if (kTok.kind !== "number" || !/^\d+$/.test(kTok.value) || Number(kTok.value) < 1) {
        return { ok: false, reason: kTok.kind === "eof" ? "syntax" : "complex" };
      }
      state.aggregationParam = kTok.value;
      this.next();
      if (this.peek().kind !== "comma") return { ok: false, reason: "syntax" };
      this.next();
    }

    const inner = this.parseRangeOrSelector(state);
    if (!inner.ok) return inner;

    if (this.peek().kind !== "rparen") {
      return { ok: false, reason: this.peek().kind === "eof" ? "syntax" : "complex" };
    }
    this.next(); // )

    const maybeTrailing = this.peek();
    if (
      maybeTrailing.kind === "ident"
      && (maybeTrailing.value.toLowerCase() === "by" || maybeTrailing.value.toLowerCase() === "without")
    ) {
      if (hasLeadingClause) return { ok: false, reason: "syntax" };
      const clause = this.parseGroupingClause();
      if (!clause.ok) return clause;
      state.aggregationMode = clause.mode;
      state.aggregationLabels = clause.labels;
    }

    return { ok: true };
  }

  private parseRangeOrSelector(
    state: PromQLBuilderState,
  ): { ok: true } | { ok: false; reason: PromQLParseFailureReason } {
    const tok = this.peek();
    if (tok.kind === "ident" && this.peek(1).kind === "lparen") {
      if (!RANGE_FUNCTION_SET.has(tok.value)) {
        return { ok: false, reason: "complex" };
      }
      return this.parseRangeCall(state, tok.value as PromQLRangeFunctionName);
    }
    return this.parseSelector(state);
  }

  private parseRangeCall(
    state: PromQLBuilderState,
    fn: PromQLRangeFunctionName,
  ): { ok: true } | { ok: false; reason: PromQLParseFailureReason } {
    this.next(); // fn name
    this.next(); // (
    state.rangeFunction = fn;

    const sel = this.parseSelector(state);
    if (!sel.ok) return sel;

    const rangeTok = this.peek();
    if (rangeTok.kind !== "range") {
      return { ok: false, reason: rangeTok.kind === "eof" ? "syntax" : "complex" };
    }
    if (!isValidPromQLRangeInterval(rangeTok.value)) {
      // Subqueries like [1h:1m] or invalid durations stay in Code mode.
      return { ok: false, reason: "complex" };
    }
    state.rangeInterval = rangeTok.value;
    this.next(); // [interval]

    if (this.peek().kind !== "rparen") {
      return { ok: false, reason: this.peek().kind === "eof" ? "syntax" : "complex" };
    }
    this.next(); // )
    return { ok: true };
  }

  private parseSelector(
    state: PromQLBuilderState,
  ): { ok: true } | { ok: false; reason: PromQLParseFailureReason } {
    const tok = this.peek();
    if (tok.kind === "ident") {
      // Reject keywords or standalone function calls masquerading as selectors.
      if (this.peek(1).kind === "lparen") {
        return { ok: false, reason: "complex" };
      }
      state.metric = this.next().value;
      if (this.peek().kind === "lbrace") {
        return this.parseLabelMatchers(state);
      }
      return { ok: true };
    }
    if (tok.kind === "lbrace") {
      return this.parseLabelMatchers(state);
    }
    return { ok: false, reason: tok.kind === "eof" || tok.kind === "rparen" ? "syntax" : "complex" };
  }

  private parseLabelMatchers(
    state: PromQLBuilderState,
  ): { ok: true } | { ok: false; reason: PromQLParseFailureReason } {
    this.next(); // {
    let extractedNameFromMatcher = false;
    let isFirstItem = true;

    while (this.peek().kind !== "rbrace") {
      const labelTok = this.peek();
      if (labelTok.kind === "eof") return { ok: false, reason: "syntax" };

      // PromQL UTF-8 shorthand: `{"metric.name"}` or `{"metric.name", label="val"}`
      if (
        isFirstItem
        && labelTok.kind === "string"
        && labelTok.quote === '"'
        && (this.peek(1).kind === "comma" || this.peek(1).kind === "rbrace")
      ) {
        if (labelTok.value.length === 0) return { ok: false, reason: "syntax" };
        if (state.metric || extractedNameFromMatcher) {
          return { ok: false, reason: "complex" };
        }
        state.metric = this.next().value;
        extractedNameFromMatcher = true;
        isFirstItem = false;
        if (this.peek().kind === "comma") {
          this.next();
        }
        continue;
      }

      isFirstItem = false;
      if (labelTok.kind === "ident") {
        this.next();
      } else if (labelTok.kind === "string" && labelTok.quote === '"') {
        if (labelTok.value.length === 0) return { ok: false, reason: "syntax" };
        this.next();
      } else {
        return { ok: false, reason: labelTok.kind === "string" ? "syntax" : "complex" };
      }

      const opTok = this.peek();
      if (opTok.kind !== "op") {
        return { ok: false, reason: "syntax" };
      }
      this.next();

      const valTok = this.peek();
      if (valTok.kind !== "string") {
        return { ok: false, reason: "syntax" };
      }
      this.next();

      const operator = opTok.value as PromQLLabelOperator;
      if (labelTok.value === "__name__" && operator === "=") {
        if (state.metric || extractedNameFromMatcher) {
          return { ok: false, reason: "complex" };
        }
        state.metric = valTok.value;
        extractedNameFromMatcher = true;
      } else {
        state.filters.push({
          id: nextBuilderFilterId(),
          label: labelTok.value,
          operator,
          value: valTok.value,
        });
      }

      if (this.peek().kind === "comma") {
        this.next();
      } else if (this.peek().kind !== "rbrace") {
        return { ok: false, reason: "syntax" };
      }
    }
    this.next(); // }
    return { ok: true };
  }
}

/**
 * Parse a PromQL expression back into a structured visual builder state.
 * Complex custom expressions (binary math, subqueries, offset/@, comments,
 * unsupported functions, double aggregations) return `{ ok: false, reason: "complex" }`
 * so callers can prompt the user to edit in Code mode without losing input.
 */
export function parsePromQLToBuilder(query: string): PromQLParseResult {
  const trimmed = query.trim();
  if (!trimmed) {
    return { ok: true, state: createEmptyBuilderState() };
  }
  const lexed = tokenizePromQL(trimmed);
  if (!lexed.ok) return lexed;
  return new BuilderParser(lexed.tokens).parse();
}

// ---------------------------------------------------------------------------
// Pure draft helpers & bidirectional CodeMirror <-> Builder controller
// ---------------------------------------------------------------------------

export function addBuilderFilter(state: PromQLBuilderState): PromQLBuilderState {
  return {
    ...state,
    filters: [
      ...state.filters,
      { id: nextBuilderFilterId(), label: "", operator: "=", value: "" },
    ],
  };
}

export function updateBuilderFilter(
  state: PromQLBuilderState,
  id: string,
  patch: Partial<Omit<PromQLLabelFilter, "id">>,
): PromQLBuilderState {
  return {
    ...state,
    filters: state.filters.map((filter) => (filter.id === id ? { ...filter, ...patch } : filter)),
  };
}

export function removeBuilderFilter(state: PromQLBuilderState, id: string): PromQLBuilderState {
  return {
    ...state,
    filters: state.filters.filter((filter) => filter.id !== id),
  };
}

export function toggleBuilderAggregationLabel(
  state: PromQLBuilderState,
  label: string,
): PromQLBuilderState {
  const normalized = label.trim();
  if (!normalized) return state;
  const exists = state.aggregationLabels.includes(normalized);
  const nextLabels = exists
    ? state.aggregationLabels.filter((item) => item !== normalized)
    : [...state.aggregationLabels, normalized];
  const nextMode: PromQLAggregationMode =
    nextLabels.length > 0 && state.aggregationMode === "none" ? "by" : state.aggregationMode;
  return {
    ...state,
    aggregationMode: nextMode,
    aggregationLabels: nextLabels,
  };
}

/**
 * Toggle or change `histogram_quantile`. When activating quantile from a raw
 * metric, a bare `rate(...)`, or an ungrouped `sum(...)`, automatically fills in
 * the standard `rate` + `sum by (le)` pipeline so non-PromQL users immediately
 * get a working query regardless of left-to-right selection order.
 */
export function applyBuilderHistogramQuantile(
  state: PromQLBuilderState,
  quantile: string,
): PromQLBuilderState {
  const nextQuantile = quantile.trim();
  if (!nextQuantile) {
    return { ...state, histogramQuantile: "" };
  }
  const activating = state.histogramQuantile.trim() === "";
  if (activating && state.aggregation === "none") {
    return {
      ...state,
      rangeFunction: state.rangeFunction === "none" ? "rate" : state.rangeFunction,
      rangeInterval: state.rangeInterval || DEFAULT_PROMQL_RANGE_INTERVAL,
      aggregation: "sum",
      aggregationMode: "by",
      aggregationLabels: ["le"],
      histogramQuantile: nextQuantile,
    };
  }
  if (activating && state.aggregation === "sum" && state.aggregationMode === "none") {
    return {
      ...state,
      aggregationMode: "by",
      aggregationLabels: ["le"],
      histogramQuantile: nextQuantile,
    };
  }
  if (
    activating
    && state.aggregation === "sum"
    && state.aggregationMode === "by"
    && !state.aggregationLabels.includes("le")
  ) {
    return {
      ...state,
      aggregationLabels: ["le", ...state.aggregationLabels],
      histogramQuantile: nextQuantile,
    };
  }
  return { ...state, histogramQuantile: nextQuantile };
}

export function createBuilderController(initialQuery: string): PromQLBuilderControllerState {
  const parsed = parsePromQLToBuilder(initialQuery);
  if (parsed.ok) {
    return {
      draft: parsed.state,
      lastSyncedQuery: initialQuery,
      status: "synced",
    };
  }
  return {
    draft: createEmptyBuilderState(),
    lastSyncedQuery: initialQuery,
    status: parsed.reason,
  };
}

export function syncBuilderControllerFromQuery(
  controller: PromQLBuilderControllerState,
  query: string,
): PromQLBuilderControllerState {
  if (query === controller.lastSyncedQuery) return controller;
  const parsed = parsePromQLToBuilder(query);
  if (parsed.ok) {
    return {
      draft: parsed.state,
      lastSyncedQuery: query,
      status: "synced",
    };
  }
  return {
    draft: controller.draft,
    lastSyncedQuery: query,
    status: parsed.reason,
  };
}

export function updateBuilderController(
  controller: PromQLBuilderControllerState,
  updater: (draft: PromQLBuilderState) => PromQLBuilderState,
): { controller: PromQLBuilderControllerState; query: string } {
  const nextDraft = updater(controller.draft);
  const nextQuery = buildPromQL(nextDraft);
  return {
    controller: {
      draft: nextDraft,
      lastSyncedQuery: nextQuery,
      status: "synced",
    },
    query: nextQuery,
  };
}

export function resetBuilderController(): {
  controller: PromQLBuilderControllerState;
  query: string;
} {
  return {
    controller: {
      draft: createEmptyBuilderState(),
      lastSyncedQuery: "",
      status: "synced",
    },
    query: "",
  };
}
