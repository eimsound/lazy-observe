import { promises as fs } from "node:fs";
import os from "node:os";
import path from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import { collectSourceMaps, inferRelease } from "../src/index";

const temporary: string[] = [];
afterEach(async () => { await Promise.all(temporary.splice(0).map((directory) => fs.rm(directory, { recursive: true, force: true }))); });

describe("inferRelease", () => {
  it("prefers an explicit release", () => expect(inferRelease(" release-1 ", "/unused", () => { throw new Error("unexpected git"); })).toBe("release-1"));
  it("turns a failing git into the clear 'set release explicitly' error", () => {
    expect(() => inferRelease(undefined, "/repo", () => { throw new Error("Command failed: git rev-parse"); })).toThrow(
      /unable to infer release from git; set release explicitly/
    );
  });
  it("uses the seven-character git sha", () => expect(inferRelease(undefined, "/repo", (cwd) => { expect(cwd).toBe("/repo"); return "1234567890abcdef\n"; })).toBe("1234567"));
});

it("collects only source maps and derives minified basenames", async () => {
  const directory = await fs.mkdtemp(path.join(os.tmpdir(), "lazy-observe-plugin-")); temporary.push(directory);
  await fs.mkdir(path.join(directory, "assets"));
  await Promise.all([
    fs.writeFile(path.join(directory, "index.html"), ""),
    fs.writeFile(path.join(directory, "assets", "app.js"), ""),
    fs.writeFile(path.join(directory, "assets", "app.js.map"), "{}"),
    fs.writeFile(path.join(directory, "style.css.map"), "{}")
  ]);
  const maps = await collectSourceMaps(directory);
  expect(maps.map(({ filename }) => filename)).toEqual(["app.js", "style.css"]);
});
