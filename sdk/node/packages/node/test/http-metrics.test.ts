import {
  AggregationTemporality,
  DataPointType,
  InMemoryMetricExporter,
  MeterProvider,
  PeriodicExportingMetricReader,
} from "@opentelemetry/sdk-metrics";
import { describe, expect, it } from "vitest";
import {
  createHTTPServerRequestDurationHistogram,
  HTTP_SERVER_REQUEST_DURATION_BUCKETS_SECONDS,
} from "../src/http-metrics.js";

describe("HTTP server metrics", () => {
  it("uses the OpenTelemetry HTTP duration bucket boundaries in seconds", async () => {
    const exporter = new InMemoryMetricExporter(AggregationTemporality.CUMULATIVE);
    const reader = new PeriodicExportingMetricReader({
      exporter,
      exportIntervalMillis: 60_000,
    });
    const provider = new MeterProvider({ readers: [reader] });

    try {
      const histogram = createHTTPServerRequestDurationHistogram(provider.getMeter("test"));
      histogram?.record(0.15);
      await provider.forceFlush();

      const metric = exporter
        .getMetrics()
        .flatMap((resource) => resource.scopeMetrics)
        .flatMap((scope) => scope.metrics)
        .find((candidate) => candidate.descriptor.name === "http.server.request.duration");
      expect(metric?.dataPointType).toBe(DataPointType.HISTOGRAM);
      if (!metric || metric.dataPointType !== DataPointType.HISTOGRAM) {
        throw new Error("HTTP server request duration histogram was not exported");
      }
      expect(metric.dataPoints[0]?.value.buckets.boundaries).toEqual(
        HTTP_SERVER_REQUEST_DURATION_BUCKETS_SECONDS,
      );
    } finally {
      await provider.shutdown();
    }
  });
});
