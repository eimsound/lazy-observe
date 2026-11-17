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
});
