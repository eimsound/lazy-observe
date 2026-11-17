import { describe, expect, it } from "vitest";
import { resolveSignals } from "../src/sdk.js";

describe("signal switches", () => {
  it("enables all signals by default", () => expect(resolveSignals({})).toEqual({ metrics: true, logs: true, traces: true }));
  it("honors every explicit switch independently", () => {
    expect(resolveSignals({ metrics: false, logs: true, traces: false })).toEqual({ metrics: false, logs: true, traces: false });
    expect(resolveSignals({ metrics: false, logs: false, traces: false })).toEqual({ metrics: false, logs: false, traces: false });
  });
});
