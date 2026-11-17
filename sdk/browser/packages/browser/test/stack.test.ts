import { describe, expect, it } from "vitest";
import { parseStack } from "../src/stack";

const origin = "https://app.example";

describe("parseStack", () => {
  it("parses Chrome frames and reverses them to outermost first", () => {
    const frames = parseStack(`TypeError: boom
    at save (https://app.example/assets/app.js:22:9)
    at https://app.example/assets/app.js:11:3
    at vendor (https://cdn.example/lib.js:7:2)`, origin);
    expect(frames).toEqual([
      { function: "vendor", filename: "https://cdn.example/lib.js", lineno: 7, colno: 2, in_app: false },
      { function: "<anonymous>", filename: "https://app.example/assets/app.js", lineno: 11, colno: 3, in_app: true },
      { function: "save", filename: "https://app.example/assets/app.js", lineno: 22, colno: 9, in_app: true }
    ]);
  });

  it("parses Firefox named, anonymous and extension frames", () => {
    const frames = parseStack(`save@https://app.example/app.js:42:13
@https://app.example/bootstrap.js:8:1
dispatch@moz-extension://abc/content.js:3:4`, origin);
    expect(frames.map((frame) => [frame.function, frame.filename, frame.in_app])).toEqual([
      ["dispatch", "moz-extension://abc/content.js", false],
      ["<anonymous>", "https://app.example/bootstrap.js", true],
      ["save", "https://app.example/app.js", true]
    ]);
  });

  it("parses Safari named, global and eval frames", () => {
    const frames = parseStack(`render@https://app.example/main.js:10:5
global code@https://app.example/index.js:4:1
eval code@https://app.example/main.js:20:7`, origin);
    expect(frames).toHaveLength(3);
    expect(frames[0]).toMatchObject({ function: "eval code", filename: "https://app.example/main.js", lineno: 20, colno: 7, in_app: true });
    expect(frames[1].function).toBe("global code");
    expect(frames[2].function).toBe("render");
  });

  it.each([
    ["named", "Error: x\n    at submit (https://app.example/form.js:30:12)", "submit", "https://app.example/form.js"],
    ["anonymous", "Error\n    at https://app.example/app.js:8:2", "<anonymous>", "https://app.example/app.js"],
    ["eval", "Error\n    at eval (eval at run (https://app.example/app.js:4:2), <anonymous>:1:1)", "eval", "<anonymous>"]
  ])("parses Chrome %s sample", (_kind, stack, fn, filename) => {
    expect(parseStack(stack, origin)[0]).toMatchObject({ function: fn, filename });
  });

  it.each([
    ["named", "submit@https://app.example/form.js:30:12", "submit", true],
    ["anonymous", "@https://app.example/app.js:8:2", "<anonymous>", true],
    ["cross-origin", "dispatch@https://static.vendor.example/lib.js:7:1", "dispatch", false]
  ])("parses Firefox %s sample", (_kind, stack, fn, inApp) => {
    expect(parseStack(stack, origin)[0]).toMatchObject({ function: fn, in_app: inApp });
  });

  it.each([
    ["named", "submit@https://app.example/form.js:30:12", "submit", "https://app.example/form.js"],
    ["global", "global code@https://app.example/index.js:2:1", "global code", "https://app.example/index.js"],
    ["extension", "callback@safari-web-extension://example/inject.js:5:6", "callback", "safari-web-extension://example/inject.js"]
  ])("parses Safari %s sample", (_kind, stack, fn, filename) => {
    expect(parseStack(stack, origin)[0]).toMatchObject({ function: fn, filename });
  });
});
