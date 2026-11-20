import { execFile } from "node:child_process";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

// 加载冒烟：包只发 ESM，必须在 Node 22.15+ 上既能 import 也能 require()（require(esm)，22.12 起默认开启），
// 且 bin 能直接执行。测的是构建产物（经 package.json 的 exports 自引用），CI 在 Node 22 / 24 / 26 上各跑一遍。
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const name = "@lazy-observe/bundler-plugin";

function node(args: string[]): Promise<{ stdout: string; stderr: string }> {
  return new Promise((resolve, reject) => {
    execFile(process.execPath, args, { cwd: root }, (error, stdout, stderr) => {
      if (error) reject(new Error(`${error.message}\n${stderr}`));
      else resolve({ stdout, stderr });
    });
  });
}

const probe = (load: string) => `
  const check = (m, keys) => JSON.stringify(keys.map((key) => typeof m[key]));
  ${load}
`;

describe(`load smoke on Node ${process.version}`, () => {
  it.each([
    ["require .", `const m = require("${name}"); console.log(check(m, ["uploadSourceMaps", "vite", "default", "inferRelease"]));`],
    ["require ./vite", `const m = require("${name}/vite"); console.log(check(m, ["vite", "default"]));`],
    ["import .", `import("${name}").then((m) => console.log(check(m, ["uploadSourceMaps", "vite", "default", "inferRelease"])));`],
    ["import ./vite", `import("${name}/vite").then((m) => console.log(check(m, ["vite", "default"])));`]
  ])("%s", async (_label, code) => {
    const { stdout } = await node(["-e", probe(code)]);
    const types = JSON.parse(stdout) as string[];
    expect(types.every((type) => type === "function")).toBe(true);
  });

  it("require() and import() resolve to the same ESM instance", async () => {
    const { stdout } = await node(["-e", `
      const required = require("${name}");
      import("${name}").then((imported) => console.log(String(required.uploadSourceMaps === imported.uploadSourceMaps)));
    `]);
    expect(stdout.trim()).toBe("true");
  });

  it("does not expose a webpack entry", async () => {
    await expect(node(["-e", `require("${name}/webpack")`])).rejects.toThrow(/ERR_PACKAGE_PATH_NOT_EXPORTED/);
  });

  it("runs the bin with --help", async () => {
    const { stdout } = await node([path.join(root, "dist", "cli.js"), "--help"]);
    expect(stdout).toContain("Usage: lazy-observe-sourcemaps upload");
  });
});
