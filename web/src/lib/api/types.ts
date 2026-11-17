/**
 * Prometheus HTTP API response shapes.
 * lazy-observe's metrics query endpoint speaks the Prometheus /query_range
 * contract (VictoriaMetrics is Prometheus-compatible). Keeping these types
 * verbatim to the upstream spec means the same UI works against VM or any
 * Prometheus-compatible endpoint (see requirements §5.2).
 */

export type MetricLabels = Record<string, string>;

/** A single [unixSeconds, sampleValueAsString] tuple. */
export type PromValueTuple = [number, string];

export interface PromMatrixResult {
  metric: MetricLabels;
  values: PromValueTuple[];
}

export interface PromMatrixData {
  resultType: "matrix";
  result: PromMatrixResult[];
}

export interface PromVectorResult {
  metric: MetricLabels;
  value: PromValueTuple;
}

export interface PromVectorData {
  resultType: "vector";
  result: PromVectorResult[];
}

export interface PromScalarData {
  resultType: "scalar";
  result: PromValueTuple;
}

export interface PromStringData {
  resultType: "string";
  result: PromValueTuple;
}

export interface PromSuccess {
  status: "success";
  data: PromMatrixData;
  warnings?: string[];
  infos?: string[];
  stats?: unknown;
}

export interface PromError {
  status: "error";
  errorType?: string;
  error: string;
}

export type PromQueryRangeResponse = PromSuccess | PromError;

export type PromInstantSuccess = {
  status: "success";
  data: PromVectorData | PromScalarData | PromMatrixData | PromStringData;
  warnings?: string[];
  infos?: string[];
  stats?: unknown;
};

export type PromInstantResponse = PromInstantSuccess | PromError;

/** Normalized series the chart layer consumes (values already parsed to numbers). */
export interface TimeSeries {
  /** Human-readable label built from the metric's labels. */
  name: string;
  labels: MetricLabels;
  /** [Unix milliseconds, parsed sample value]. */
  points: Array<[number, number | null]>;
}

export interface QueryRangeParams {
  query: string;
  /** Unix seconds. */
  start: number;
  /** Unix seconds. */
  end: number;
  /** Step in seconds. */
  step: number;
}

export interface MetricSample {
  name: string;
  labels: MetricLabels;
  timestamp: number;
  value: number | null;
}

export interface MetricTableValue {
  timestamp: number;
  raw: string;
  value: number | null;
}

export interface MetricTableRow {
  name: string;
  labels: MetricLabels;
  values: MetricTableValue[];
}

export interface MetricTableResult {
  resultType: "vector" | "matrix" | "scalar" | "string";
  rows: MetricTableRow[];
}

export interface MetricsQueryEnvelope<T> {
  data: T;
  warnings: string[];
  infos: string[];
  durationMs: number;
}
