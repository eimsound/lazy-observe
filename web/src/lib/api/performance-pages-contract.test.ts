import assert from "node:assert/strict";
import { test } from "vitest";

import {
  normalizeVitalPagesResponse,
  requestVitalPages,
} from "./performance-pages-contract.ts";

test("normalizes project-scoped Web Vital series into sorted unique pages", () => {
  assert.deepEqual(
    normalizeVitalPagesResponse({
      status: "success",
      data: [
        { __name__: "web_vitals_lcp", project_id: "alpha", page: "/orders/:id" },
        { __name__: "web_vitals_cls", project_id: "alpha", page: "/checkout" },
        { __name__: "web_vitals_inp", project_id: "alpha", page: "/orders/:id" },
        { __name__: "web_vitals_fcp", project_id: "alpha" },
      ],
    }),
    ["/checkout", "/orders/:id"],
  );
});

test("accepts an empty series result and rejects backend or malformed envelopes", () => {
  assert.deepEqual(normalizeVitalPagesResponse({ status: "success", data: [] }), []);
  assert.throws(
    () => normalizeVitalPagesResponse({ status: "error", error: "query unavailable" }),
    /query unavailable/,
  );
  assert.throws(
    () => normalizeVitalPagesResponse({ status: "success", data: [{ page: 42 }] }),
    /label page must be a string/,
  );
  assert.throws(
    () => normalizeVitalPagesResponse({ status: "success", data: {} }),
    /data must be an array/,
  );
});

test("forwards cancellation to the page discovery transport without wrapping it", async () => {
  const controller = new AbortController();
  let receivedSignal: AbortSignal | undefined;
  const pending = requestVitalPages((signal) => {
    receivedSignal = signal;
    return new Promise((_resolve, reject) => {
      signal?.addEventListener(
        "abort",
        () => reject(signal.reason ?? new DOMException("aborted", "AbortError")),
        { once: true },
      );
    });
  }, controller.signal);

  controller.abort();
  await assert.rejects(pending, (error: unknown) => error instanceof DOMException && error.name === "AbortError");
  assert.equal(receivedSignal, controller.signal);
});
