import { describe, expect, it } from "vitest";
import { extractTraceparent } from "../src/sdk.js";

describe("traceparent inheritance", () => {
  it("extracts the browser trace and parent span IDs", () => {
    const parent = extractTraceparent({ traceparent: "00-11111111111111111111111111111111-2222222222222222-01" });
    expect(parent).toMatchObject({ traceId: "11111111111111111111111111111111", spanId: "2222222222222222", traceFlags: 1, isRemote: true });
  });

  it("ignores invalid traceparent", () => expect(extractTraceparent({ traceparent: "broken" })).toBeUndefined());
});
