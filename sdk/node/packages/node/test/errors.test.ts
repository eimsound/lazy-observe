import { describe, expect, it } from "vitest";
import {
  errorEvent,
  MAX_ERROR_EVENT_BYTES,
  parseV8Stack,
  sanitizeErrorEvent,
  sanitizeUrl,
  trimErrorEventToBudget,
} from "../src/errors.js";
import { MAX_MESSAGE_LENGTH, MAX_STACK_FRAMES } from "../src/sanitize.js";

describe("parseV8Stack", () => {
  it("parses async and dependency frames in outermost-first order", () => {
    const stack = `Error: boom
    at async handler (/srv/app/routes.js:20:7)
    at dispatch (/srv/app/node_modules/express/lib/router.js:10:2)
    at main (file:///srv/app/main.js:30:1)`;
    expect(parseV8Stack(stack)).toEqual([
      expect.objectContaining({ function: "main", filename: "/srv/app/main.js", lineno: 30, in_app: true }),
      expect.objectContaining({ function: "dispatch", module: "express", in_app: false }),
      expect.objectContaining({ function: "async handler", lineno: 20, colno: 7, in_app: true }),
    ]);
  });

  it("handles malformed file:// URLs and invalid percent-encoding without throwing URIError or TypeError", () => {
    const stack = `Error: bad paths
    at a (file:///srv/app/%ZZ/bad.js:12:4)
    at b (file://[invalid/path.js:9:1)
    at c (/srv/app/line-only.js:42)`;
    expect(() => parseV8Stack(stack)).not.toThrow();
    const frames = parseV8Stack(stack);
    expect(frames).toEqual([
      expect.objectContaining({ function: "c", filename: "/srv/app/line-only.js", lineno: 42 }),
      expect.objectContaining({ function: "b", filename: "file://[invalid/path.js", lineno: 9, colno: 1 }),
      expect.objectContaining({ function: "a", filename: "/srv/app/%ZZ/bad.js", lineno: 12, colno: 4 }),
    ]);
    expect(frames[0]?.colno).toBeUndefined();
  });
});

describe("sanitize pipeline and errorEvent budget", () => {
  it("strips credentials, query strings, and fragments from URLs and relative paths", () => {
    expect(sanitizeUrl("https://alice:secret@api.example/pay?card=4111#step2")).toBe("https://api.example/pay");
    expect(sanitizeUrl("/checkout?token=abc#frag")).toBe("/checkout");
    expect(sanitizeUrl("https://alice:secret@[bad-host/pay?token=abc#frag")).toBe("https://[bad-host/pay");
  });

  it("formats non-Error objects instead of [object Object], strips secrets, caps fields/frames, and enforces 200KB budget", () => {
    const objEvent = errorEvent({ code: "E_CARD", reason: "declined" });
    expect(objEvent.message).not.toBe("[object Object]");
    expect(objEvent.message).toContain("declined");

    const manyFrames = Array.from({ length: 80 }, (_, index) => ({
      function: `fn_${index}`,
      filename: `https://user:pw@app.example/assets/chunk-${index}.js?sig=secret#hash`,
      abs_path: `https://user:pw@app.example/assets/chunk-${index}.js?sig=secret#hash`,
      lineno: index + 1,
      in_app: true,
    }));
    const rawEvent = errorEvent(new Error("e".repeat(20_000)), {
      url: "https://user:pw@api.example/orders?secret=123#frag",
      tags: { detail: "d".repeat(10_000) },
    });
    rawEvent.exception.stacktrace.frames = manyFrames;

    const sanitized = sanitizeErrorEvent(rawEvent);
    expect(sanitized).toBeDefined();
    expect(sanitized?.url).toBe("https://api.example/orders");
    expect(sanitized?.message.length).toBeLessThanOrEqual(MAX_MESSAGE_LENGTH + "...[truncated]".length);
    expect(sanitized?.exception.stacktrace.frames).toHaveLength(MAX_STACK_FRAMES);
    expect(sanitized?.exception.stacktrace.frames[0]?.function).toBe("fn_30");
    expect(sanitized?.exception.stacktrace.frames[0]?.filename).toBe("https://app.example/assets/chunk-30.js");
    expect(JSON.stringify(sanitized)).not.toContain("secret");

    const huge = errorEvent(new Error("e".repeat(160_000)), {
      tags: { detail: "d".repeat(120_000) },
    });
    const trimmed = trimErrorEventToBudget(huge);
    expect(trimmed).toBeDefined();
    expect(Buffer.byteLength(JSON.stringify(trimmed), "utf8")).toBeLessThanOrEqual(MAX_ERROR_EVENT_BYTES);
  });
});
