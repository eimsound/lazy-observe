import { describe, expect, it } from "vitest";
import {
  buildReplayHTTPMarker,
  buildReplayVitalMarker,
  REPLAY_CUSTOM_EVENT_TAGS,
  replayRequestMethod,
} from "../src/replay";

describe("Replay marker wire contract", () => {
  it("records only safe HTTP metadata and strips URL secrets", () => {
    const marker = buildReplayHTTPMarker(
      "post",
      "https://alice:secret@example.test/api/orders/42?token=private#card",
      201,
      12.3456,
      1_721_234_567_890,
    );

    expect(REPLAY_CUSTOM_EVENT_TAGS.http).toBe("lazy-observe:http");
    expect(marker).toEqual({
      timestamp: 1_721_234_567_890,
      method: "POST",
      path: "/api/orders/42",
      status: 201,
      duration_ms: 12.35,
    });
    expect(JSON.stringify(marker)).not.toContain("private");
    expect(JSON.stringify(marker)).not.toContain("secret");
    expect(marker).not.toHaveProperty("body");
    expect(marker).not.toHaveProperty("response");
  });

  it("preserves Request methods without exposing Request bodies", () => {
    const request = new Request("https://example.test/checkout?session=secret", {
      method: "PATCH",
      body: "card=4111111111111111",
    });
    expect(replayRequestMethod(request)).toBe("PATCH");
    expect(buildReplayHTTPMarker(replayRequestMethod(request), request, 0, 5, 10)).toEqual({
      timestamp: 10,
      method: "PATCH",
      path: "/checkout",
      status: 0,
      duration_ms: 5,
    });
  });

  it("still returns a path when an absolute URL is malformed", () => {
    const marker = buildReplayHTTPMarker(
      "GET",
      "https://alice:secret@[invalid/api/orders?token=private",
      0,
      1,
      10,
    );
    expect(marker.path).toBe("/api/orders");
    expect(JSON.stringify(marker)).not.toMatch(/alice|secret|private/);
  });

  it("records the documented Web Vitals fields", () => {
    expect(REPLAY_CUSTOM_EVENT_TAGS.vital).toBe("lazy-observe:vital");
    expect(buildReplayVitalMarker({
      timestamp: 1_721_234_567_891,
      name: "LCP",
      value: 2450.5,
      rating: "needs-improvement",
    })).toEqual({
      timestamp: 1_721_234_567_891,
      name: "LCP",
      value: 2450.5,
      rating: "needs-improvement",
    });
  });

  it("uses stable tags for all three timeline marker classes", () => {
    expect(REPLAY_CUSTOM_EVENT_TAGS).toEqual({
      error: "lazy-observe:error",
      http: "lazy-observe:http",
      vital: "lazy-observe:vital",
    });
  });
});
