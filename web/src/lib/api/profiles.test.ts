import assert from "node:assert/strict";
import { afterEach, test, vi } from "vitest";

import { fetchRawProfileBlob, getFlamegraph } from "./profiles.ts";
import { ApiError } from "./client.ts";

const originalWindow = globalThis.window;

function installOriginWindow() {
  Object.defineProperty(globalThis, "window", {
    configurable: true,
    value: { location: { origin: "http://localhost:5173" } },
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
  if (originalWindow === undefined) {
    Reflect.deleteProperty(globalThis, "window");
  } else {
    Object.defineProperty(globalThis, "window", {
      configurable: true,
      value: originalWindow,
    });
  }
});

test("getFlamegraph maps HTTP 404 to null so the page clears stale flamegraphs", async () => {
  installOriginWindow();
  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      new Response(JSON.stringify({ error: "no profiles in window" }), {
        status: 404,
        headers: { "Content-Type": "application/json" },
      }),
    ),
  );

  const result = await getFlamegraph({
    service: "checkout",
    type: "heap",
    start: 1_752_758_400,
    end: 1_752_762_000,
  });
  assert.equal(result, null);
});

test("getFlamegraph still throws ApiError on non-404 failures", async () => {
  installOriginWindow();
  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      new Response(JSON.stringify({ error: "storage unavailable" }), {
        status: 503,
        headers: { "Content-Type": "application/json" },
      }),
    ),
  );

  await assert.rejects(
    () =>
      getFlamegraph({
        service: "checkout",
        type: "cpu",
        start: 1_752_758_400,
        end: 1_752_762_000,
      }),
    (err: unknown) => err instanceof ApiError && err.status === 503,
  );
});

test("fetchRawProfileBlob attaches X-LO-Project header for non-default projects", async () => {
  installOriginWindow();
  const captured: Array<{ url: string; headers: Record<string, string>; credentials?: RequestCredentials }> = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
      captured.push({
        url: String(input),
        headers: (init?.headers ?? {}) as Record<string, string>,
        credentials: init?.credentials,
      });
      return new Response(new Uint8Array([0x1f, 0x8b, 0x08]), {
        status: 200,
        headers: { "Content-Type": "application/octet-stream" },
      });
    }),
  );

  const blob = await fetchRawProfileBlob(
    {
      id: "42",
      service: "checkout",
      type: "cpu",
      startMs: 1_752_758_400_000,
      endMs: 1_752_762_000_000,
      size: 1024,
    },
    "staging",
  );

  assert.equal(blob.size, 3);
  assert.equal(captured.length, 1);
  assert.equal(captured[0]?.url, "/api/profiles/42/raw");
  assert.equal(captured[0]?.headers["X-LO-Project"], "staging");
  assert.equal(captured[0]?.credentials, "include");
});
