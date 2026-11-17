export interface EndpointPointDTO {
  /** Bucket start as Unix milliseconds. */
  time: number;
  /** Requests per second observed in this bucket. */
  throughput: number;
  /** Latency percentiles in milliseconds. */
  p50: number;
  p95: number;
  p99: number;
  /** Error fraction in [0, 1]. */
  error_rate: number;
}

export interface EndpointStatDTO {
  service: string;
  method: string;
  route: string;
  /** Requests per second observed over the requested window. */
  throughput: number;
  /** Latency percentiles in milliseconds. */
  p50: number;
  p95: number;
  p99: number;
  /** Error fraction in [0, 1]. */
  error_rate: number;
  series: EndpointPointDTO[];
}

export interface EndpointStatsResponseDTO {
  /** Requested window bounds as Unix milliseconds. */
  start: number;
  end: number;
  step_seconds: number;
  endpoints: EndpointStatDTO[];
  truncated: boolean;
}

export type EndpointPoint = EndpointPointDTO;
export type EndpointStat = EndpointStatDTO;
export type EndpointStatsResult = EndpointStatsResponseDTO;

/** Validate the gateway envelope before values reach sorting and chart code. */
export function normalizeEndpointStatsResponse(value: unknown): EndpointStatsResult {
  const response = objectValue(value, "performance response");
  const start = integerValue(response.start, "start");
  const end = integerValue(response.end, "end");
  const stepSeconds = integerValue(response.step_seconds, "step_seconds");
  if (end <= start) throw new TypeError("performance response end must be after start");
  if (stepSeconds <= 0) throw new TypeError("performance response step_seconds must be positive");
  if (!Array.isArray(response.endpoints)) {
    throw new TypeError("performance response must contain endpoints");
  }
  if (typeof response.truncated !== "boolean") {
    throw new TypeError("performance response truncated must be boolean");
  }
  return {
    start,
    end,
    step_seconds: stepSeconds,
    endpoints: response.endpoints.map((endpoint, index) => normalizeEndpoint(endpoint, index)),
    truncated: response.truncated,
  };
}

function normalizeEndpoint(value: unknown, index: number): EndpointStat {
  const endpoint = objectValue(value, `endpoints[${index}]`);
  if (!Array.isArray(endpoint.series)) {
    throw new TypeError(`performance response endpoints[${index}].series must be an array`);
  }
  return {
    service: nonEmptyString(endpoint.service, `endpoints[${index}].service`),
    method: nonEmptyString(endpoint.method, `endpoints[${index}].method`),
    route: nonEmptyString(endpoint.route, `endpoints[${index}].route`),
    throughput: nonNegativeNumber(endpoint.throughput, `endpoints[${index}].throughput`),
    p50: nonNegativeNumber(endpoint.p50, `endpoints[${index}].p50`),
    p95: nonNegativeNumber(endpoint.p95, `endpoints[${index}].p95`),
    p99: nonNegativeNumber(endpoint.p99, `endpoints[${index}].p99`),
    error_rate: rateValue(endpoint.error_rate, `endpoints[${index}].error_rate`),
    series: endpoint.series.map((point, pointIndex) =>
      normalizePoint(point, `endpoints[${index}].series[${pointIndex}]`),
    ),
  };
}

function normalizePoint(value: unknown, path: string): EndpointPoint {
  const point = objectValue(value, path);
  return {
    time: integerValue(point.time, `${path}.time`),
    throughput: nonNegativeNumber(point.throughput, `${path}.throughput`),
    p50: nonNegativeNumber(point.p50, `${path}.p50`),
    p95: nonNegativeNumber(point.p95, `${path}.p95`),
    p99: nonNegativeNumber(point.p99, `${path}.p99`),
    error_rate: rateValue(point.error_rate, `${path}.error_rate`),
  };
}

function objectValue(value: unknown, path: string): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new TypeError(`${path} must be an object`);
  }
  return value as Record<string, unknown>;
}

function nonEmptyString(value: unknown, path: string): string {
  if (typeof value !== "string" || value.trim() === "") {
    throw new TypeError(`performance response ${path} must be a non-empty string`);
  }
  return value;
}

function integerValue(value: unknown, path: string): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value)) {
    throw new TypeError(`performance response ${path} must be a safe integer`);
  }
  return value;
}

function nonNegativeNumber(value: unknown, path: string): number {
  if (typeof value !== "number" || !Number.isFinite(value) || value < 0) {
    throw new TypeError(`performance response ${path} must be a non-negative finite number`);
  }
  return value;
}

function rateValue(value: unknown, path: string): number {
  const rate = nonNegativeNumber(value, path);
  if (rate > 1) throw new TypeError(`performance response ${path} must be at most 1`);
  return rate;
}
