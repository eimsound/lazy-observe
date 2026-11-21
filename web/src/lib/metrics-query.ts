import dayjs from "./dayjs.ts";

export type MetricsDisplay = "graph" | "table";
export type MetricsQueryTab = MetricsDisplay;
export type MetricsGraphMode = "lines" | "stacked";
export type MetricsTableMode = "series" | "labels";
export type MetricsTimeFormat = "readable" | "timestamp";

export type MetricsResolution =
  | { type: "auto"; density: "low" | "medium" | "high" }
  | { type: "fixed"; step: number }
  | { type: "custom"; step: number };

export interface RelativeMetricsRange {
  mode: "relative";
  presetId: string;
  seconds: number;
}

export interface AbsoluteMetricsRange {
  mode: "absolute";
  startMs: number;
  /** Missing means "up to now" and is resolved at execution time. */
  endMs?: number;
}

export type MetricsRange = RelativeMetricsRange | AbsoluteMetricsRange;

export interface ResolvedMetricsRange {
  start: number;
  end: number;
  step: number;
}

/**
 * Metadata autocomplete is bucketed by local calendar days, matching vmui.
 * The wider, stable bucket improves cache reuse while still excluding metric
 * and label values that only exist outside the days currently being queried.
 */
export function metricsAutocompleteRange(
  range: Pick<ResolvedMetricsRange, "start" | "end">,
): Pick<ResolvedMetricsRange, "start" | "end"> {
  return {
    start: dayjs(range.start * 1_000).startOf("day").valueOf() / 1_000,
    end: dayjs(range.end * 1_000).endOf("day").valueOf() / 1_000,
  };
}

export function formatMetricsAxisValue(value: number): string {
  const absolute = Math.abs(value);
  if (absolute >= 1_000_000_000_000_000_000_000_000_000) {
    return value.toExponential(2);
  }
  const units: Array<[number, string]> = [
    [1_000_000_000_000_000_000_000_000, "Y"],
    [1_000_000_000_000_000_000_000, "Z"],
    [1_000_000_000_000_000_000, "E"],
    [1_000_000_000_000_000, "P"],
    [1_000_000_000_000, "T"],
    [1_000_000_000, "G"],
    [1_000_000, "M"],
    [1_000, "k"],
  ];
  for (const [divisor, suffix] of units) {
    if (absolute >= divisor) {
      return `${Number((value / divisor).toPrecision(4))}${suffix}`;
    }
  }
  if (absolute > 0 && absolute < 0.001) return value.toExponential(2);
  return String(Number(value.toPrecision(4)));
}

export function metricsLabelColumns(
  rows: ReadonlyArray<{ labels: Record<string, string> }>,
): string[] {
  const names = new Set<string>();
  rows.forEach((row) => {
    Object.keys(row.labels).forEach((name) => {
      if (name !== "__name__") names.add(name);
    });
  });
  return [...names].sort((left, right) => left.localeCompare(right));
}

/**
 * An empty selection means every series is visible. A plain click isolates a
 * series (and clicking that isolated row again resets all); a modifier click
 * toggles one series without discarding the rest of the current selection.
 */
export function toggleMetricsSeriesSelection(
  current: readonly number[],
  index: number,
  seriesCount: number,
  multiple: boolean,
): number[] {
  if (!multiple) return current.length === 1 && current[0] === index ? [] : [index];
  if (current.length === 0) {
    return Array.from({ length: seriesCount }, (_, itemIndex) => itemIndex)
      .filter((itemIndex) => itemIndex !== index);
  }
  return current.includes(index)
    ? current.filter((itemIndex) => itemIndex !== index)
    : [...current, index].sort((left, right) => left - right);
}

export function estimateClockDriftSeconds(
  serverSeconds: number,
  requestStartedMs: number,
  responseReceivedMs: number,
): number {
  const clientMidpointSeconds = (requestStartedMs + responseReceivedMs) / 2_000;
  return Math.abs(serverSeconds - clientMidpointSeconds);
}

export const METRICS_RANGE_PRESETS: RelativeMetricsRange[] = [
  { mode: "relative", presetId: "15m", seconds: 15 * 60 },
  { mode: "relative", presetId: "1h", seconds: 60 * 60 },
  { mode: "relative", presetId: "6h", seconds: 6 * 60 * 60 },
  { mode: "relative", presetId: "24h", seconds: 24 * 60 * 60 },
  { mode: "relative", presetId: "7d", seconds: 7 * 24 * 60 * 60 },
  { mode: "relative", presetId: "30d", seconds: 30 * 24 * 60 * 60 },
  { mode: "relative", presetId: "3mo", seconds: 90 * 24 * 60 * 60 },
];

export const METRICS_RANGE_STEPS = [
  60,
  5 * 60,
  15 * 60,
  30 * 60,
  60 * 60,
  2 * 60 * 60,
  6 * 60 * 60,
  12 * 60 * 60,
  24 * 60 * 60,
  2 * 24 * 60 * 60,
  7 * 24 * 60 * 60,
  14 * 24 * 60 * 60,
  30 * 24 * 60 * 60,
  90 * 24 * 60 * 60,
  180 * 24 * 60 * 60,
  365 * 24 * 60 * 60,
] as const;

export function effectiveMetricsStep(resolution: MetricsResolution, rangeSeconds: number): number {
  if (resolution.type !== "auto") return Math.max(1, Math.floor(resolution.step));
  const points = resolution.density === "high" ? 750 : resolution.density === "medium" ? 250 : 100;
  return Math.max(1, Math.floor(rangeSeconds / points));
}

export function formatPrometheusDuration(seconds: number): string {
  const value = Math.max(1, Math.floor(seconds));
  const units: Array<[number, string]> = [
    [365 * 24 * 60 * 60, "y"],
    [7 * 24 * 60 * 60, "w"],
    [24 * 60 * 60, "d"],
    [60 * 60, "h"],
    [60, "m"],
    [1, "s"],
  ];
  let rest = value;
  let output = "";
  for (const [unitSeconds, suffix] of units) {
    const amount = Math.floor(rest / unitSeconds);
    if (amount > 0) {
      output += `${amount}${suffix}`;
      rest %= unitSeconds;
    }
  }
  return output || "1s";
}

export function parsePrometheusDuration(value: string): number | null {
  const text = value.trim();
  if (!text) return null;
  const unitSeconds: Record<string, number> = {
    y: 365 * 24 * 60 * 60,
    w: 7 * 24 * 60 * 60,
    d: 24 * 60 * 60,
    h: 60 * 60,
    m: 60,
    s: 1,
    ms: 0.001,
  };
  const pattern = /(\d+(?:\.\d+)?)(ms|[ywdhms])/g;
  let total = 0;
  let consumed = "";
  for (const match of text.matchAll(pattern)) {
    total += Number(match[1]) * unitSeconds[match[2]];
    consumed += match[0];
  }
  return consumed === text && Number.isFinite(total) && total >= 1 ? Math.floor(total) : null;
}

export function metricsRangeSeconds(range: MetricsRange, nowMs = dayjs().valueOf()): number {
  if (range.mode === "relative") return range.seconds;
  return Math.max(1, Math.floor(((range.endMs ?? nowMs) - range.startMs) / 1000));
}

export function setMetricsRangeSeconds(
  range: MetricsRange,
  seconds: number,
  nowMs = dayjs().valueOf(),
): MetricsRange {
  const safeSeconds = Math.max(1, Math.floor(seconds));
  if (range.mode === "relative") {
    return { mode: "relative", presetId: formatPrometheusDuration(safeSeconds), seconds: safeSeconds };
  }
  const endMs = range.endMs ?? nowMs;
  return { mode: "absolute", startMs: endMs - safeSeconds * 1000, endMs: range.endMs };
}

export function stepMetricsRange(
  range: MetricsRange,
  direction: -1 | 1,
  nowMs = dayjs().valueOf(),
): MetricsRange {
  const current = metricsRangeSeconds(range, nowMs);
  const candidates = direction > 0 ? METRICS_RANGE_STEPS : [...METRICS_RANGE_STEPS].reverse();
  const next = candidates.find((seconds) => direction > 0 ? seconds > current : seconds < current);
  return next === undefined ? range : setMetricsRangeSeconds(range, next, nowMs);
}

export function shiftMetricsRange(
  range: MetricsRange,
  direction: -1 | 1,
  nowMs = dayjs().valueOf(),
): MetricsRange {
  const durationMs = metricsRangeSeconds(range, nowMs) * 1000;
  const delta = direction * durationMs / 2;
  if (range.mode === "relative") {
    const endMs = nowMs + delta;
    return { mode: "absolute", startMs: endMs - durationMs, endMs };
  }
  const endMs = (range.endMs ?? nowMs) + delta;
  return { mode: "absolute", startMs: range.startMs + delta, endMs };
}

export function resolveMetricsRange(
  range: MetricsRange,
  nowMs = dayjs().valueOf(),
): ResolvedMetricsRange {
  const endMs = range.mode === "relative" ? nowMs : (range.endMs ?? nowMs);
  const startMs = range.mode === "relative" ? endMs - range.seconds * 1000 : range.startMs;
  if (!Number.isFinite(startMs) || !Number.isFinite(endMs) || startMs >= endMs) {
    throw new Error("invalid metrics time range");
  }
  const seconds = Math.max(1, Math.floor((endMs - startMs) / 1000));
  return {
    start: Math.floor(startMs / 1000),
    end: Math.floor(endMs / 1000),
    step: Math.max(1, Math.ceil(seconds / 300)),
  };
}

export function dateParts(ms: number, local: boolean) {
  const value = local ? dayjs(ms) : dayjs.utc(ms);
  return {
    date: value.format("YYYY-MM-DD"),
    time: value.format("HH:mm:ss"),
  };
}

export function parseDateTime(dateText: string, timeText: string, local: boolean): number | null {
  const normalizedTime = /^\d{2}:\d{2}$/.test(timeText.trim())
    ? `${timeText.trim()}:00`
    : timeText.trim();
  const input = `${dateText.trim()} ${normalizedTime}`;
  const value = local
    ? dayjs(input, "YYYY-MM-DD HH:mm:ss", true)
    : dayjs.utc(input, "YYYY-MM-DD HH:mm:ss", true);
  return value.isValid() ? value.valueOf() : null;
}

export function formatRangeLabel(range: MetricsRange, local: boolean, nowLabel: string): string {
  if (range.mode === "relative") return range.presetId;
  const start = dateParts(range.startMs, local);
  const end = range.endMs === undefined ? null : dateParts(range.endMs, local);
  return `${start.date} ${start.time} → ${end ? `${end.date} ${end.time}` : nowLabel}`;
}

export interface CalendarCell {
  key: string;
  day: number;
  monthOffset: -1 | 0 | 1;
  ms: number;
}

export function calendarCells(year: number, month: number, local: boolean): CalendarCell[] {
  const input = `${year}-${month + 1}-1`;
  const monthStart = local
    ? dayjs(input, "YYYY-M-D", true)
    : dayjs.utc(input, "YYYY-M-D", true);
  const firstDay = (monthStart.day() + 6) % 7;
  return Array.from({ length: 42 }, (_, index) => {
    const dayOffset = index - firstDay + 1;
    const value = monthStart.date(dayOffset);
    const ms = value.valueOf();
    const cellYear = value.year();
    const cellMonth = value.month();
    const cellDay = value.date();
    const delta = cellYear === year ? cellMonth - month : cellYear < year ? -1 : 1;
    return {
      key: `${cellYear}-${cellMonth}-${cellDay}`,
      day: cellDay,
      monthOffset: Math.max(-1, Math.min(1, delta)) as -1 | 0 | 1,
      ms,
    };
  });
}

export type SuggestionKind = "metric" | "function" | "label" | "value";

export interface PromQLSuggestion {
  value: string;
  kind: SuggestionKind;
}

export interface PromQLContext {
  kind: "metric" | "label" | "value" | "none";
  token: string;
  tokenStart: number;
  metric: string;
  label: string;
  matchers: string[];
}

const EXPRESSION_TAIL_BOUNDARY =
  /(?<![\w.:/-])(?:fill|group_left|group_right)\s*\([^)]*\)\s*|\s(?:or|and|unless|default|ifnot|if|group_left|group_right|fill)\s|}|\+|-|\*|\/|\^/i;
const TRAILING_MODIFIER_CALL = /(?<![\w.:/-])(?:fill|group_left|group_right)\s*\([^)]*\)\s+$/i;

function expressionTail(beforeCursor: string): string {
  const parts = beforeCursor.split(EXPRESSION_TAIL_BOUNDARY);
  return parts.at(-1)?.trim() ?? "";
}

export function analyzePromQLContext(query: string, cursor: number): PromQLContext {
  const before = query.slice(0, cursor);
  const tail = expressionTail(before);
  const metric = /^\s*([^{}(),\s]+)(?=\{|$)/.exec(tail)?.[1] ?? "";
  const labelMatches = [...tail.matchAll(/([a-z_:-][\w\-.:/]*)\s*(?:=|!=|=~|!~)\s*"[^"]*"/g)];
  const currentLabel = [...tail.matchAll(/([a-z_:-][\w\-.:/]*)\s*(?:=|!=|=~|!~)\s*"?[^"]*$/g)].at(-1)?.[1] ?? "";
  const valueMatch = /(?:=|!=|=~|!~)\s*"?([^"]*)$/.exec(before);
  if (currentLabel && valueMatch) {
    const token = valueMatch[1] ?? "";
    return {
      kind: "value",
      token,
      tokenStart: cursor - token.length,
      metric,
      label: currentLabel,
      matchers: labelMatches.filter((match) => match[1] !== currentLabel).map((match) => match[0]),
    };
  }
  const isLabel = /(?:\b(?:by|without|on|ignoring)\s*\([^)]*|\{[^}]*)$/i.test(before);
  const token = /([\w_.:/-]*)$/.exec(before)?.[1] ?? "";
  if (isLabel) {
    return {
      kind: "label",
      token,
      tokenStart: cursor - token.length,
      metric,
      label: "",
      matchers: labelMatches.map((match) => match[0]),
    };
  }
  const closed = /[})"'`]\s*$/.test(before) && !TRAILING_MODIFIER_CALL.test(before);
  return {
    kind: closed ? "none" : "metric",
    token,
    tokenStart: cursor - token.length,
    metric,
    label: "",
    matchers: [],
  };
}

export function insertPromQLSuggestion(
  query: string,
  cursor: number,
  context: PromQLContext,
  suggestion: PromQLSuggestion,
) {
  let insert = suggestion.value;
  let suffix = "";
  if (suggestion.kind === "function") suffix = "(";
  if (suggestion.kind === "value") {
    insert = insert.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
    const prefix = query.slice(0, context.tokenStart);
    const hasOpeningQuote = /"$/.test(prefix);
    const hasClosingQuote = query.slice(cursor).trimStart().startsWith('"');
    insert = `${hasOpeningQuote ? "" : '"'}${insert}${hasClosingQuote ? "" : '"'}`;
  }
  const next = `${query.slice(0, context.tokenStart)}${insert}${suffix}${query.slice(cursor)}`;
  return { query: next, cursor: context.tokenStart + insert.length + suffix.length };
}

// Generated from the h4 headings in vmui v1.156.0's bundled MetricsQL.md.
export const METRICSQL_FUNCTIONS = `
abs absent absent_over_time acos acosh aggr_over_time alias any ascent_over_time asin asinh atan atanh avg avg_over_time
bitmap_and bitmap_or bitmap_xor bottomk bottomk_avg bottomk_last bottomk_max bottomk_median bottomk_min buckets_limit
ceil changes changes_prometheus clamp clamp_max clamp_min cos cosh count count_eq_over_time count_gt_over_time count_le_over_time count_ne_over_time count_over_time count_values count_values_over_time
day_of_month day_of_week day_of_year days_in_month decreases_over_time default_rollup deg delta delta_prometheus deriv deriv_fast descent_over_time distinct distinct_over_time drop_common_labels drop_empty_series duration_over_time
end exp first_over_time floor geomean geomean_over_time group histogram histogram_avg histogram_fraction histogram_over_time histogram_quantile histogram_quantiles histogram_share histogram_stddev histogram_stdvar hoeffding_bound_lower hoeffding_bound_upper holt_winters hour
idelta ideriv increase increase_prometheus increase_pure increases_over_time integrate interpolate irate keep_last_value keep_next_value label_copy label_del label_graphite_group label_join label_keep label_lowercase label_map label_match label_mismatch label_move label_replace label_set label_transform label_uppercase label_value labels_equal lag last_over_time lifetime limit_offset limitk ln log10 log2
mad mad_over_time max max_over_time median median_over_time min min_over_time minute mode mode_over_time month now outlier_iqr_over_time outliers_iqr outliers_mad outliersk pi predict_linear present_over_time prometheus_buckets quantile quantile_over_time quantiles quantiles_over_time rad rand rand_exponential rand_normal range_avg range_first range_last range_linear_regression range_mad range_max range_median range_min range_normalize range_over_time range_quantile range_stddev range_stdvar range_sum range_trim_outliers range_trim_spikes range_trim_zscore range_zscore rate rate_prometheus rate_over_sum remove_resets resets rollup rollup_candlestick rollup_delta rollup_deriv rollup_increase rollup_rate rollup_scrape_interval round ru running_avg running_max running_min running_sum scalar scrape_interval sgn share share_eq_over_time share_gt_over_time share_le_over_time sin sinh smooth_exponential sort sort_by_label sort_by_label_desc sort_by_label_numeric sort_by_label_numeric_desc sort_desc sqrt stale_samples_over_time start stddev stddev_over_time stdvar stdvar_over_time step sum sum2 sum2_over_time sum_eq_over_time sum_gt_over_time sum_le_over_time sum_over_time tan tanh tfirst_over_time time timestamp timestamp_with_name timezone_offset tlast_change_over_time tlast_over_time tmax_over_time tmin_over_time topk topk_avg topk_last topk_max topk_median topk_min ttf union vector year zscore zscore_over_time
`.trim().split(/\s+/);

export interface MetricsHistoryEntry {
  query: string;
  at: number;
}

const historyKey = (project: string) => `lo-metrics-history:${project || "default"}`;

export function getMetricsHistory(project: string): MetricsHistoryEntry[] {
  try {
    const parsed = JSON.parse(localStorage.getItem(historyKey(project)) ?? "[]") as unknown;
    if (!Array.isArray(parsed)) return [];
    return parsed.flatMap((entry) => {
      if (typeof entry === "string") return [{ query: entry, at: 0 }];
      if (entry && typeof entry === "object" && typeof (entry as MetricsHistoryEntry).query === "string") {
        return [{ query: (entry as MetricsHistoryEntry).query, at: Number((entry as MetricsHistoryEntry).at) || 0 }];
      }
      return [];
    }).slice(0, 50);
  } catch {
    return [];
  }
}

export function pushMetricsHistory(project: string, queries: string[], now = dayjs().valueOf()) {
  const current = getMetricsHistory(project);
  const next = queries.reduce((entries, raw) => {
    const query = raw.trim();
    if (!query) return entries;
    return [{ query, at: now }, ...entries.filter((entry) => entry.query !== query)];
  }, current).slice(0, 50);
  try {
    localStorage.setItem(historyKey(project), JSON.stringify(next));
  } catch {
    // Query execution must not fail because browser storage is unavailable.
  }
  return next;
}

export function clearMetricsHistory(project: string) {
  try {
    localStorage.removeItem(historyKey(project));
  } catch {
    // Ignore unavailable storage.
  }
  return [] as MetricsHistoryEntry[];
}
