import type { Histogram, Meter } from "@opentelemetry/api";

export const HTTP_SERVER_REQUEST_DURATION_BUCKETS_SECONDS: number[] = [
  0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10,
];

export function createHTTPServerRequestDurationHistogram(
  meter: Meter | undefined,
): Histogram | undefined {
  return meter?.createHistogram("http.server.request.duration", {
    unit: "s",
    description: "Duration of HTTP server requests.",
    advice: {
      explicitBucketBoundaries: HTTP_SERVER_REQUEST_DURATION_BUCKETS_SECONDS,
    },
  });
}
