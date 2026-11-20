import { inspect } from "node:util";
import { describe, expect, it } from "vitest";
import { parseDSN } from "../src/dsn.js";

describe("parseDSN", () => {
  it("parses endpoint, slug and token", () => {
    expect(parseDSN("https://observe.example:3100/p/my-app@lo_secret")).toEqual({
      endpoint: "https://observe.example:3100", project: "my-app", token: "lo_secret",
    });
  });

  it.each([
    "ftp://host/p/app@token", "http://host/app@token", "http://host/p/app", "http://host/p/app@token/extra",
    "http://user@host/p/app@token", "http://host/p/app@token?q=1", "http://host/p/app@token#x",
  ])("rejects invalid DSN %s", (dsn) => expect(() => parseDSN(dsn)).toThrow(/parse DSN/));

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
});
