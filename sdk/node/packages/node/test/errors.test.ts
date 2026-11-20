import { describe, expect, it } from "vitest";
import { errorEvent, MAX_ERROR_EVENT_BYTES, parseV8Stack, trimErrorEventToBudget } from "../src/errors.js";

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

describe("errorEvent budget and formatting", () => {
  it("formats non-Error objects instead of [object Object] and trims events over 200KB", () => {
    const objEvent = errorEvent({ code: "E_CARD", reason: "declined" });
    expect(objEvent.message).not.toBe("[object Object]");
    expect(objEvent.message).toContain("declined");

    const huge = errorEvent(new Error("e".repeat(160_000)), {
      tags: { detail: "d".repeat(120_000) },
    });
    const trimmed = trimErrorEventToBudget(huge);
    expect(trimmed).toBeDefined();
    expect(Buffer.byteLength(JSON.stringify(trimmed), "utf8")).toBeLessThanOrEqual(MAX_ERROR_EVENT_BYTES);
  });
});
