import { EventEmitter } from "node:events";
import express from "express";
import { afterEach, describe, expect, it, vi } from "vitest";
import { init, parseWinstonLevel, resolveExpressRoute } from "../src/sdk.js";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("Node SDK integrations & unhandled errors", () => {
  it("captureUnhandledErrors does not suppress Node unhandledRejection semantics and flushes uncaughtExceptionMonitor immediately", async () => {
    const beforeReject = process.listenerCount("unhandledRejection");
    const beforeMonitor = process.listenerCount("uncaughtExceptionMonitor");

    const fetcher = vi.fn(async () => new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetcher);

    const sdk = init({
      dsn: "https://observe.example/p/unhandled@lo_tok",
      metrics: false,
      logs: false,
      traces: false,
      profiling: false,
      captureUnhandledErrors: true,
    });

    expect(process.listenerCount("unhandledRejection")).toBe(beforeReject);
    expect(process.listenerCount("uncaughtExceptionMonitor")).toBe(beforeMonitor + 1);

    const listeners = process.listeners("uncaughtExceptionMonitor");
    const installed = listeners[listeners.length - 1] as (err: Error, origin: string) => void;
    installed(new Error("fatal crash"), "uncaughtException");

    // Must invoke fetch synchronously/immediately without waiting for the 100ms flush timer.
    expect(fetcher).toHaveBeenCalledTimes(1);

    await sdk.shutdown({ timeoutMs: 500 });
    expect(process.listenerCount("uncaughtExceptionMonitor")).toBe(beforeMonitor);
  });

  it("expressError strips query string from URL and always calls next(error) even with malformed file:// stack", async () => {
    const fetcher = vi.fn(async (_input: string | URL | Request, _init?: RequestInit) => new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetcher);

    const sdk = init({
      dsn: "https://observe.example/p/express-err@lo_tok",
      metrics: false,
      logs: false,
      traces: false,
      profiling: false,
    });

    const err = new Error("business failure");
    err.stack = "Error: business failure\n    at broken (file:///srv/app/%ZZ/bad.js:12:3)";

    const req = {
      protocol: "https",
      hostname: "api.example",
      originalUrl: "/checkout/pay?token=secret_query_val&card=4111",
      url: "/checkout/pay?token=secret_query_val",
      get: (header: string) => (header.toLowerCase() === "host" ? "api.example" : undefined),
    } as unknown as express.Request;
    const res = new EventEmitter() as unknown as express.Response;
    const next = vi.fn();

    sdk.expressError()(err, req, res, next);
    expect(next).toHaveBeenCalledTimes(1);
    expect(next).toHaveBeenCalledWith(err);

    await sdk.shutdown({ timeoutMs: 500 });
    expect(fetcher).toHaveBeenCalledTimes(1);
    const body = JSON.parse(String((fetcher.mock.calls[0]?.[1] as RequestInit | undefined)?.body)) as {
      events: Array<{ url?: string }>;
    };
    expect(body.events[0]?.url).toBe("https://api.example/checkout/pay");
    expect(body.events[0]?.url).not.toContain("secret_query_val");
  });

  it("express combines req.baseUrl + req.route.path and uses low-cardinality /* for unmatched 404 routes", async () => {
    const sdk = init({
      dsn: "https://observe.example/p/express-routes@lo_tok",
      metrics: false,
      logs: false,
      traces: false,
      profiling: false,
    });
    const app = express();
    app.use(sdk.express());

    const router = express.Router();
    const observedRoutes: string[] = [];
    router.get("/users/:id", (req, res) => {
      observedRoutes.push(resolveExpressRoute(req));
      res.json({ id: req.params.id });
    });
    app.use("/api/v1", router);
    app.use((req, res) => {
      observedRoutes.push(resolveExpressRoute(req));
      res.status(404).end();
    });

    const server = await new Promise<import("node:http").Server>((resolve) => {
      const s = app.listen(0, "127.0.0.1", () => resolve(s));
    });
    const port = (server.address() as import("node:net").AddressInfo).port;
    try {
      const okRes = await fetch(`http://127.0.0.1:${port}/api/v1/users/42`);
      expect(okRes.status).toBe(200);

      const notFoundRes = await fetch(`http://127.0.0.1:${port}/unmatched/secret-id-999`);
      expect(notFoundRes.status).toBe(404);
      expect(observedRoutes).toEqual(["/api/v1/users/:id", "/*"]);
    } finally {
      await new Promise<void>((resolve) => server.close(() => resolve()));
      await sdk.shutdown({ timeoutMs: 500 });
    }
  });

  it("pino preserves Error objects and works with teeToStdout: false; winston handles ANSI colorized levels, circular objects and BigInt", async () => {
    const stdoutSpy = vi.spyOn(process.stdout, "write").mockImplementation(() => true);
    const sdk = init({
      dsn: "https://observe.example/p/loggers@lo_tok",
      metrics: false,
      logs: false,
      traces: false,
      profiling: false,
      teeToStdout: false,
    });

    // Pino with teeToStdout: false (must not open /dev/null) and direct Error logging
    const capturedPinoArgs: unknown[][] = [];
    const logger = sdk.pino({
      level: "info",
      hooks: {
        logMethod(args, method) {
          capturedPinoArgs.push(args);
          method.apply(this, args);
        },
      },
    });
    const boom = new Error("pino boom");
    expect(() => logger.error(boom, "request failed")).not.toThrow();
    expect(capturedPinoArgs).toHaveLength(1);
    const firstArg = capturedPinoArgs[0]?.[0] as { err?: Error } | undefined;
    expect(firstArg?.err).toBe(boom);
    expect(firstArg?.err?.message).toBe("pino boom");

    // Winston colorized level parsing
    expect(parseWinstonLevel({ level: "\u001b[31merror\u001b[39m" })).toBe(50);
    expect(parseWinstonLevel({ level: "\u001b[33mwarn\u001b[39m" })).toBe(40);
    expect(parseWinstonLevel({ [Symbol.for("level")]: "error", level: "\u001b[31merror\u001b[39m" })).toBe(50);

    // Winston with teeToStdout: true, ANSI colorized level, circular object, and BigInt
    const sdkStdout = init({
      dsn: "https://observe.example/p/loggers-winston@lo_tok",
      metrics: false,
      logs: false,
      traces: false,
      profiling: false,
      teeToStdout: true,
    });
    const transport = sdkStdout.winston();
    const circular: Record<string, unknown> = {
      level: "\u001b[31merror\u001b[39m",
      message: "winston boom",
      big: 42n,
    };
    circular.self = circular;

    let done = false;
    expect(() => transport.log!(circular, () => { done = true; })).not.toThrow();
    expect(done).toBe(true);
    const written = stdoutSpy.mock.calls.map(([chunk]) => String(chunk)).join("");
    expect(written).toContain("winston boom");
    expect(written).toContain("[Circular]");
    expect(written).toContain("42");

    await sdk.shutdown({ timeoutMs: 500 });
    await sdkStdout.shutdown({ timeoutMs: 500 });
  });
});
