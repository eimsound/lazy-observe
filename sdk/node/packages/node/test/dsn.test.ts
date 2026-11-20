import { inspect } from "node:util";
import { describe, expect, it } from "vitest";
import { parseDSN, redactDSN } from "../src/dsn.js";

describe("parseDSN", () => {
  it("parses endpoint, slug and token (including optional trailing slash)", () => {
    expect(parseDSN("https://observe.example:3100/p/my-app@lo_secret")).toEqual({
      endpoint: "https://observe.example:3100", project: "my-app", token: "lo_secret",
    });
    expect(parseDSN("https://observe.example:3100/p/my-app@lo_secret/")).toEqual({
      endpoint: "https://observe.example:3100", project: "my-app", token: "lo_secret",
    });
  });

  it.each([
    "", "   ",
    "ftp://host/p/app@token", "http://host/app@token", "http://host/p/app", "http://host/p/app@token/extra",
    "http://user@host/p/app@token", "http://user:pass@host/p/app@token",
    "http://host/p/app@token?q=1", "http://host/p/app@token#x",
    "http://host/p/app@a@b", "http://host/p/%20@lo_tok", "http://host/p/app@%20",
  ])("rejects invalid DSN %j", (dsn) => expect(() => parseDSN(dsn)).toThrow(/parse DSN/));

  it.each([
    "https://observe.example/p/app@los_upload_secret_999",
    "https://observe.example/p/app@lou_user_secret_999",
  ])("rejects non-ingest token prefix %s without leaking token", (badDsn) => {
    let caught: unknown;
    try {
      parseDSN(badDsn);
    } catch (error) {
      caught = error;
    }
    expect(caught).toBeInstanceOf(Error);
    expect((caught as Error).message).toMatch(/project API token/);
    expect((caught as Error).message).not.toContain("secret_999");
  });

  it("enforces requiredTokenPrefix when requested", () => {
    expect(parseDSN("https://observe.example/p/app@lo_abc", { requiredTokenPrefix: "lo_" }).token).toBe("lo_abc");
    expect(() => parseDSN("https://observe.example/p/app@plain", { requiredTokenPrefix: "lo_" })).toThrow(/must start with lo_/);
    expect(() => parseDSN("https://observe.example/p/app@lo_", { requiredTokenPrefix: "lo_" })).toThrow(/must start with lo_/);
  });

  it.each([
    "not-a-valid-url/p/app@lo_secret_leak_check_123",
    "https://observe.example/p/app@lo_secret_leak_check_123%ZZ",
  ])("never leaks plaintext token via error message, cause, or cause.input for %s", (badDsn) => {
    let caught: unknown;
    try {
      parseDSN(badDsn);
    } catch (error) {
      caught = error;
    }
    expect(caught).toBeInstanceOf(Error);
    const err = caught as Error & { cause?: unknown };
    expect(err.cause).toBeUndefined();
    expect(err.message).not.toContain("lo_secret_leak_check_123");
    expect(inspect(err, { depth: 5 })).not.toContain("lo_secret_leak_check_123");
  });

  it("redacts tokens cleanly in redactDSN", () => {
    expect(redactDSN("https://observe.example:3100/p/my-app@lo_top_secret")).toBe(
      "https://observe.example:3100/p/my-app@[redacted]",
    );
    expect(redactDSN("not-a-dsn@lo_top_secret")).toBe("[invalid DSN]");
  });
});
