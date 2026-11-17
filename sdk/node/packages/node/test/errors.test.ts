import { describe, expect, it } from "vitest";
import { parseV8Stack } from "../src/errors.js";

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
});
