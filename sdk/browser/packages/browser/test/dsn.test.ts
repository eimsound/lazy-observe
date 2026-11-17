import { describe, expect, it } from "vitest";
import { parseDsn } from "../src/dsn";

describe("parseDsn", () => {
  it("extracts endpoint, project and token", () => {
    expect(parseDsn("http://host:3100/p/myapp@secret-token")).toEqual({ endpoint: "http://host:3100", project: "myapp", token: "secret-token" });
  });
  it("accepts https and encoded values", () => {
    expect(parseDsn("https://observe.example/p/my%20app@tok%2Ben")).toEqual({ endpoint: "https://observe.example", project: "my app", token: "tok+en" });
  });
  it.each(["nope", "ftp://host/p/a@b", "http://host/a@b", "http://host/p/a@b?x=1"])("rejects %s", (dsn) => expect(() => parseDsn(dsn)).toThrow());
});
