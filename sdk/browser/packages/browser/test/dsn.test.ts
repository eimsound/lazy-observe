import { describe, expect, it } from "vitest";
import { parseDsn, redactDsn } from "../src/dsn";

describe("parseDsn", () => {
  it("extracts endpoint, project and token (including optional trailing slash)", () => {
    expect(parseDsn("http://host:3100/p/myapp@secret-token")).toEqual({
      endpoint: "http://host:3100",
      project: "myapp",
      token: "secret-token",
    });
    expect(parseDsn("http://host:3100/p/myapp@lo_secret/")).toEqual({
      endpoint: "http://host:3100",
      project: "myapp",
      token: "lo_secret",
    });
  });

  it("accepts https and encoded values", () => {
    expect(parseDsn("https://observe.example/p/my%20app@tok%2Ben")).toEqual({
      endpoint: "https://observe.example",
      project: "my app",
      token: "tok+en",
    });
  });

  it.each([
    "", "   ", "nope", "ftp://host/p/a@b", "http://host/a@b", "http://host/p/a@b?x=1",
    "http://host/p/a@b#frag", "http://user:pw@host/p/a@b", "http://host/p/a@b@c",
    "http://host/p/%20@lo_tok", "http://host/p/a@%20", "http://host/p/a@%ZZ",
  ])("rejects invalid DSN %j without leaking token", (dsn) => {
    let caught: unknown;
    try {
      parseDsn(dsn);
    } catch (error) {
      caught = error;
    }
    expect(caught).toBeInstanceOf(Error);
    expect((caught as Error & { cause?: unknown }).cause).toBeUndefined();
  });

  it.each([
    "https://observe.example/p/app@los_upload_secret_999",
    "https://observe.example/p/app@lou_user_secret_999",
  ])("rejects non-ingest token prefix %s without leaking token", (badDsn) => {
    let caught: unknown;
    try {
      parseDsn(badDsn);
    } catch (error) {
      caught = error;
    }
    expect(caught).toBeInstanceOf(Error);
    expect((caught as Error).message).toMatch(/project API token/);
    expect((caught as Error).message).not.toContain("secret_999");
  });

  it("enforces requiredTokenPrefix when requested and redacts tokens via redactDsn", () => {
    expect(parseDsn("https://observe.example/p/app@lo_abc", { requiredTokenPrefix: "lo_" }).token).toBe("lo_abc");
    expect(() => parseDsn("https://observe.example/p/app@plain", { requiredTokenPrefix: "lo_" })).toThrow(/must start with lo_/);
    expect(redactDsn("https://observe.example/p/my-app@lo_secret_123")).toBe(
      "https://observe.example/p/my-app@[redacted]",
    );
    expect(redactDsn("invalid@lo_secret_123")).toBe("[invalid DSN]");
  });
});
