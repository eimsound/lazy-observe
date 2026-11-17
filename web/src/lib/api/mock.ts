import type { PromQueryRangeResponse, QueryRangeParams } from "./types";

/**
 * Offline mock backend (VITE_MOCK=1). Generates deterministic multi-series
 * sine-wave data so the whole UI is developable/demoable without the Fiber
 * gateway running. Series count/labels are derived from the query string so
 * different queries look different.
 */

function hashString(s: string): number {
  let h = 2166136261;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return Math.abs(h);
}

const MOCK_INSTANCES = [
  "10.0.0.11:9100",
  "10.0.0.12:9100",
  "10.0.0.13:9100",
  "10.0.0.14:9100",
];
const MOCK_JOBS = ["api", "worker", "gateway"];

export function mockQueryRange(params: QueryRangeParams): PromQueryRangeResponse {
  const { query, start, end, step } = params;

  // An empty query yields an error, mirroring real backend behaviour.
  if (!query.trim()) {
    return { status: "error", errorType: "bad_data", error: "empty query" };
  }

  // "__empty" is a magic token to exercise the empty-state UI.
  if (query.includes("__empty")) {
    return { status: "success", data: { resultType: "matrix", result: [] } };
  }
  // "__error" exercises the error-state UI.
  if (query.includes("__error")) {
    return {
      status: "error",
      errorType: "execution",
      error: `unknown function or metric: ${query}`,
    };
  }

  const seed = hashString(query);
  const seriesCount = (seed % 3) + 3; // 3..5 series
  const points = Math.max(2, Math.floor((end - start) / step) + 1);

  const result = Array.from({ length: seriesCount }, (_, s) => {
    const instance = MOCK_INSTANCES[(seed + s) % MOCK_INSTANCES.length];
    const job = MOCK_JOBS[(seed + s) % MOCK_JOBS.length];
    const amplitude = 40 + ((seed >> (s + 1)) % 30);
    const baseline = 50 + ((seed >> s) % 40);
    const phase = (s * Math.PI) / seriesCount + (seed % 7);
    const period = 60 * ((seed % 5) + 4); // seconds per cycle

    const values: Array<[number, string]> = Array.from({ length: points }, (_, i) => {
      const t = start + i * step;
      const wave = Math.sin((2 * Math.PI * (t - start)) / period + phase);
      const noise = Math.sin(t * 0.7 + s) * 3;
      const v = baseline + amplitude * wave + noise;
      return [t, v.toFixed(3)];
    });

    return {
      metric: {
        __name__: query.split("(")[0].split("{")[0].trim() || "mock_series",
        instance,
        job,
        series: String(s + 1),
      },
      values,
    };
  });

  return { status: "success", data: { resultType: "matrix", result } };
}
