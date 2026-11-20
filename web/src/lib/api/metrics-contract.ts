import type {
  MetricLabels,
  MetricTableResult,
  PromValueTuple,
  TimeSeries,
} from "./types.ts";

/**
 * Strict wire-contract validators for Prometheus / VictoriaMetrics endpoints
 * exposed through `/api/v1/{query,query_range,labels,label/:name/values,prettify-query}`
 * (`internal/gateway/gateway.go`).
 */

export interface NormalizedMetricsPayload<T> {
  data: T;
  warnings: string[];
  infos: string[];
}

export class MetricsContractError extends Error {
  status: number;
  errorType?: string;

  constructor(message: string, status = 400, errorType?: string) {
    super(message);
    this.name = "MetricsContractError";
    this.status = status;
    this.errorType = errorType;
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function assertRecord(value: unknown, label: string): Record<string, unknown> {
  if (!isRecord(value)) {
    throw new TypeError(`${label} must be an object`);
  }
  return value;
}

function normalizeStringList(value: unknown): string[] {
  if (!Array.isArray(value)) return [];
  return value.filter((item): item is string => typeof item === "string");
}

function normalizeLabels(value: unknown, label: string): MetricLabels {
  if (value === undefined || value === null) return {};
  const raw = assertRecord(value, label);
  const out: MetricLabels = {};
  for (const [k, v] of Object.entries(raw)) {
    if (typeof v === "string") {
      out[k] = v;
    } else if (v !== null && v !== undefined) {
      out[k] = String(v);
    }
  }
  return out;
}

function normalizeSampleTuple(value: unknown, label: string): PromValueTuple {
  if (!Array.isArray(value) || value.length < 2) {
    throw new TypeError(`${label} must be a [timestamp, value] tuple`);
  }
  const timestamp = typeof value[0] === "number" ? value[0] : Number(value[0]);
  if (!Number.isFinite(timestamp)) {
    throw new TypeError(`${label}[0] must be a finite unix timestamp`);
  }
  const rawVal = value[1];
  if (typeof rawVal !== "string" && typeof rawVal !== "number") {
    throw new TypeError(`${label}[1] must be a string or number`);
  }
  return [timestamp, String(rawVal)];
}

function finiteValue(raw: string): number | null {
  const parsed = Number(raw);
  return Number.isFinite(parsed) ? parsed : null;
}

/** Build a stable, readable series label from Prometheus labels. */
export function formatPromSeriesLabel(metric: Record<string, string>): string {
  const name = metric.__name__ ?? "";
  const rest = Object.entries(metric)
    .filter(([k]) => k !== "__name__")
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([k, v]) => `${k}="${v}"`)
    .join(", ");
  if (!rest) return name || "{}";
  return name ? `${name}{${rest}}` : `{${rest}}`;
}

function assertPromSuccessEnvelope(
  value: unknown,
  label: string,
): { root: Record<string, unknown>; warnings: string[]; infos: string[] } {
  const root = assertRecord(value, label);
  if (root.status === "error") {
    const message =
      typeof root.error === "string" && root.error.trim() !== ""
        ? root.error
        : "query failed";
    const errorType = typeof root.errorType === "string" ? root.errorType : undefined;
    throw new MetricsContractError(message, 400, errorType);
  }
  if (root.status !== "success") {
    throw new TypeError(`${label}.status must be "success" or "error"`);
  }
  return {
    root,
    warnings: normalizeStringList(root.warnings),
    infos: normalizeStringList(root.infos),
  };
}

/** Validate and normalize a `/api/v1/query_range` Prometheus matrix response. */
export function normalizePromRangeResponse(
  value: unknown,
): NormalizedMetricsPayload<TimeSeries[]> {
  const { root, warnings, infos } = assertPromSuccessEnvelope(value, "query_range response");
  const data = assertRecord(root.data, "query_range.data");
  if (data.resultType !== "matrix") {
    throw new TypeError(`query_range.data.resultType must be "matrix", got ${String(data.resultType)}`);
  }
  if (!Array.isArray(data.result)) {
    throw new TypeError("query_range.data.result must be an array");
  }

  const series: TimeSeries[] = data.result.map((entry, idx) => {
    const item = assertRecord(entry, `query_range.data.result[${idx}]`);
    const labels = normalizeLabels(item.metric, `query_range.data.result[${idx}].metric`);
    if (!Array.isArray(item.values)) {
      throw new TypeError(`query_range.data.result[${idx}].values must be an array`);
    }
    const points = item.values.map((sample, sampleIdx) => {
      const [ts, raw] = normalizeSampleTuple(
        sample,
        `query_range.data.result[${idx}].values[${sampleIdx}]`,
      );
      return [ts * 1000, finiteValue(raw)] as [number, number | null];
    });
    return {
      name: formatPromSeriesLabel(labels),
      labels,
      points,
    };
  });

  return { data: series, warnings, infos };
}

/** Validate and normalize a `/api/v1/query` Prometheus instant response. */
export function normalizePromInstantResponse(
  value: unknown,
): NormalizedMetricsPayload<MetricTableResult> {
  const { root, warnings, infos } = assertPromSuccessEnvelope(value, "query response");
  const data = assertRecord(root.data, "query.data");
  const resultType = data.resultType;

  if (resultType === "scalar" || resultType === "string") {
    const [timestamp, raw] = normalizeSampleTuple(data.result, "query.data.result");
    return {
      data: {
        resultType,
        rows: [
          {
            name: resultType,
            labels: {},
            values: [{ timestamp: timestamp * 1000, raw, value: finiteValue(raw) }],
          },
        ],
      },
      warnings,
      infos,
    };
  }

  if (resultType === "matrix") {
    if (!Array.isArray(data.result)) {
      throw new TypeError("query.data.result must be an array for matrix resultType");
    }
    const rows = data.result.map((entry, idx) => {
      const item = assertRecord(entry, `query.data.result[${idx}]`);
      const labels = normalizeLabels(item.metric, `query.data.result[${idx}].metric`);
      if (!Array.isArray(item.values)) {
        throw new TypeError(`query.data.result[${idx}].values must be an array`);
      }
      return {
        name: formatPromSeriesLabel(labels),
        labels,
        values: item.values.map((sample, sampleIdx) => {
          const [timestamp, raw] = normalizeSampleTuple(
            sample,
            `query.data.result[${idx}].values[${sampleIdx}]`,
          );
          return { timestamp: timestamp * 1000, raw, value: finiteValue(raw) };
        }),
      };
    });
    return { data: { resultType: "matrix", rows }, warnings, infos };
  }

  if (resultType === "vector") {
    if (!Array.isArray(data.result)) {
      throw new TypeError("query.data.result must be an array for vector resultType");
    }
    const rows = data.result.map((entry, idx) => {
      const item = assertRecord(entry, `query.data.result[${idx}]`);
      const labels = normalizeLabels(item.metric, `query.data.result[${idx}].metric`);
      const [timestamp, raw] = normalizeSampleTuple(
        item.value,
        `query.data.result[${idx}].value`,
      );
      return {
        name: formatPromSeriesLabel(labels),
        labels,
        values: [{ timestamp: timestamp * 1000, raw, value: finiteValue(raw) }],
      };
    });
    return { data: { resultType: "vector", rows }, warnings, infos };
  }

  throw new TypeError(`unsupported query.data.resultType: ${String(resultType)}`);
}

/** Validate and normalize `/api/v1/labels` and `/api/v1/label/:name/values`. */
export function normalizePromStringsResponse(value: unknown): string[] {
  const { root } = assertPromSuccessEnvelope(value, "metrics metadata response");
  if (root.data === undefined || root.data === null) return [];
  if (!Array.isArray(root.data)) {
    throw new TypeError("metrics metadata response data must be an array");
  }
  return root.data.filter((item): item is string => typeof item === "string");
}

/** Validate and normalize `/api/v1/prettify-query`. */
export function normalizePrettifyResponse(value: unknown): string {
  const root = assertRecord(value, "prettify-query response");
  if (root.status !== "success" || typeof root.query !== "string") {
    const msg =
      typeof root.msg === "string" && root.msg.trim() !== ""
        ? root.msg
        : typeof root.error === "string" && root.error.trim() !== ""
          ? root.error
          : "invalid PromQL";
    throw new MetricsContractError(msg, 400);
  }
  return root.query;
}
