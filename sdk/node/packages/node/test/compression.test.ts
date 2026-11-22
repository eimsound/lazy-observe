import { EventEmitter } from "node:events";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { gunzipSync } from "node:zlib";
import type express from "express";
import { afterEach, describe, expect, it } from "vitest";
import { init, resolveCompression, type LazyObserve } from "../src/index.js";

const ENV_KEYS = [
  "OTEL_EXPORTER_OTLP_COMPRESSION",
  "OTEL_EXPORTER_OTLP_METRICS_COMPRESSION",
  "OTEL_EXPORTER_OTLP_LOGS_COMPRESSION",
  "OTEL_EXPORTER_OTLP_TRACES_COMPRESSION",
] as const;

const savedEnv = new Map<string, string | undefined>(
  ENV_KEYS.map((key) => [key, process.env[key]]),
);

afterEach(() => {
  for (const key of ENV_KEYS) {
    const prev = savedEnv.get(key);
    if (prev === undefined) delete process.env[key];
    else process.env[key] = prev;
  }
});

interface CapturedRequest {
  encoding: string | undefined;
  body: Buffer;
}

function emitAllSignals(sdk: LazyObserve, label: string): void {
  const req = {
    method: "GET",
    path: "/health",
    url: "/health",
    protocol: "http",
    hostname: "localhost",
    httpVersion: "1.1",
    headers: {},
    baseUrl: "",
    route: { path: "/health" },
  } as unknown as express.Request;
  const res = new EventEmitter() as unknown as express.Response;
  res.statusCode = 200;
  sdk.express()(req, res, () => {
    res.emit("finish");
  });
  sdk.pino().info(label);
}

async function startCaptureServer(): Promise<{
  origin: string;
  requests: Map<string, CapturedRequest[]>;
  close(): Promise<void>;
}> {
  const requests = new Map<string, CapturedRequest[]>();
  const server = http.createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (chunk: Buffer) => chunks.push(chunk));
    req.on("end", () => {
      const url = req.url ?? "";
      const list = requests.get(url) ?? [];
      const header = req.headers["content-encoding"];
      list.push({
        encoding: Array.isArray(header) ? header.join(", ") : header,
        body: Buffer.concat(chunks),
      });
      requests.set(url, list);
      res.writeHead(200);
      res.end();
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address() as AddressInfo;
  return {
    origin: `http://127.0.0.1:${port}`,
    requests,
    close: () => new Promise<void>((resolve) => server.close(() => resolve())),
  };
}

describe("Node SDK compression config and wire encoding", () => {
  it("defaults to none and follows explicit > signal env > shared env precedence", () => {
    expect(resolveCompression({}, "traces", {})).toBe("none");
    expect(resolveCompression({ compression: "" }, "logs", {})).toBe("none");
    expect(
      resolveCompression({}, "traces", { OTEL_EXPORTER_OTLP_COMPRESSION: "gzip" }),
    ).toBe("gzip");
    expect(
      resolveCompression({}, "metrics", {
        OTEL_EXPORTER_OTLP_COMPRESSION: "gzip",
        OTEL_EXPORTER_OTLP_METRICS_COMPRESSION: "none",
      }),
    ).toBe("none");
    expect(
      resolveCompression({ compression: "none" }, "traces", {
        OTEL_EXPORTER_OTLP_COMPRESSION: "gzip",
        OTEL_EXPORTER_OTLP_TRACES_COMPRESSION: "invalid-env",
      }),
    ).toBe("none");
    expect(
      resolveCompression({ compression: " GZIP " as unknown as "gzip" }, "logs", {
        OTEL_EXPORTER_OTLP_LOGS_COMPRESSION: "none",
      }),
    ).toBe("gzip");
  });

  it("rejects invalid explicit or active env compression values", () => {
    expect(() =>
      init({
        dsn: "http://127.0.0.1:3100/p/app@lo_tok",
        compression: "zstd" as unknown as "none",
        profiling: false,
      }),
    ).toThrow(/compression/);

    process.env.OTEL_EXPORTER_OTLP_COMPRESSION = "br";
    expect(() =>
      init({
        dsn: "http://127.0.0.1:3100/p/app@lo_tok",
        profiling: false,
      }),
    ).toThrow(/OTEL_EXPORTER_OTLP_COMPRESSION/);
  });

  it("sends uncompressed by default, gzip when enabled, and honors explicit none over env", async () => {
    for (const key of ENV_KEYS) delete process.env[key];
    const sink = await startCaptureServer();
    try {
      // 1. Default -> uncompressed across traces, metrics, logs
      const sdkDefault = init({
        dsn: `${sink.origin}/p/app@lo_tok`,
        serviceName: "compress-default",
        profiling: false,
        teeToStdout: false,
      });
      emitAllSignals(sdkDefault, "default log");
      await sdkDefault.shutdown({ timeoutMs: 2_000 });

      for (const path of ["/v1/traces", "/v1/metrics", "/v1/logs"]) {
        const list = sink.requests.get(path) ?? [];
        expect(list.length).toBeGreaterThanOrEqual(1);
        for (const item of list) expect(item.encoding).toBeUndefined();
      }

      // 2. Explicit gzip -> Content-Encoding: gzip with valid gzip body on all signals
      sink.requests.clear();
      const sdkGzip = init({
        dsn: `${sink.origin}/p/app@lo_tok`,
        serviceName: "compress-gzip",
        compression: "gzip",
        profiling: false,
        teeToStdout: false,
      });
      emitAllSignals(sdkGzip, "gzip log");
      await sdkGzip.shutdown({ timeoutMs: 2_000 });

      for (const path of ["/v1/traces", "/v1/metrics", "/v1/logs"]) {
        const list = sink.requests.get(path) ?? [];
        expect(list.length).toBeGreaterThanOrEqual(1);
        for (const item of list) {
          expect(item.encoding).toBe("gzip");
          expect(gunzipSync(item.body).length).toBeGreaterThan(0);
        }
      }

      // 3. Env precedence + explicit none override
      sink.requests.clear();
      process.env.OTEL_EXPORTER_OTLP_COMPRESSION = "gzip";
      process.env.OTEL_EXPORTER_OTLP_METRICS_COMPRESSION = "none";
      const sdkEnv = init({
        dsn: `${sink.origin}/p/app@lo_tok`,
        serviceName: "compress-env",
        profiling: false,
        teeToStdout: false,
      });
      emitAllSignals(sdkEnv, "env log");
      await sdkEnv.shutdown({ timeoutMs: 2_000 });

      expect(sink.requests.get("/v1/metrics")?.[0]?.encoding).toBeUndefined();
      expect(sink.requests.get("/v1/traces")?.[0]?.encoding).toBe("gzip");
      expect(sink.requests.get("/v1/logs")?.[0]?.encoding).toBe("gzip");

      sink.requests.clear();
      process.env.OTEL_EXPORTER_OTLP_TRACES_COMPRESSION = "gzip";
      process.env.OTEL_EXPORTER_OTLP_METRICS_COMPRESSION = "gzip";
      process.env.OTEL_EXPORTER_OTLP_LOGS_COMPRESSION = "gzip";
      const sdkOverrideNone = init({
        dsn: `${sink.origin}/p/app@lo_tok`,
        serviceName: "compress-override-none",
        compression: "none",
        profiling: false,
        teeToStdout: false,
      });
      emitAllSignals(sdkOverrideNone, "override none log");
      await sdkOverrideNone.shutdown({ timeoutMs: 2_000 });

      for (const path of ["/v1/traces", "/v1/metrics", "/v1/logs"]) {
        const list = sink.requests.get(path) ?? [];
        expect(list.length).toBeGreaterThanOrEqual(1);
        for (const item of list) expect(item.encoding).toBeUndefined();
      }
    } finally {
      await sink.close();
    }
  });
});
