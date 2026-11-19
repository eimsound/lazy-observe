import { promises as fs } from "node:fs";
import path from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import { vite } from "../src/index";
import defaultExport from "../src/vite";
import { dsnFor, makeOutputDir, startServer } from "./helpers";

const cleanups: Array<() => Promise<void>> = [];
afterEach(async () => { await Promise.all(cleanups.splice(0).map((cleanup) => cleanup())); });

/** 按 Vite 的顺序驱动插件钩子：config → configResolved → writeBundle（this 是 Rollup 的插件上下文）。 */
async function build(plugin: ReturnType<typeof vite>, dir: string, command = "build") {
  const warnings: string[] = [];
  plugin.configResolved({ root: path.dirname(dir), command, build: { outDir: dir } });
  await plugin.writeBundle.call({ warn: (message) => warnings.push(message) });
  return warnings;
}

async function setup(status = 200) {
  const server = await startServer(status);
  const dir = await makeOutputDir();
  cleanups.push(server.close, () => fs.rm(dir, { recursive: true, force: true }));
  return { server, dir };
}

describe("vite plugin", () => {
  it("is both the named and the default export", () => expect(defaultExport).toBe(vite));

  it("defines __LAZY_OBSERVE_RELEASE__ from the explicit release", () => {
    const plugin = vite({ dsn: "ignored", release: " vite-1 " });
    expect(plugin.name).toBe("lazy-observe-sourcemaps");
    expect(plugin.config({})).toEqual({ define: { __LAZY_OBSERVE_RELEASE__: '"vite-1"' } });
  });

  it("uploads the output directory after a build", async () => {
    const { server, dir } = await setup();
    const warnings = await build(vite({ dsn: dsnFor(server.origin), release: "vite-1" }), dir);
    expect(warnings).toEqual([]);
    expect(server.requests.map((request) => request.url)).toEqual(["/api/releases/vite-1/sourcemaps"]);
  });

  it("warns and lets the build succeed when no DSN is configured", async () => {
    const { server, dir } = await setup();
    const warnings = await build(vite({ release: "vite-1" }), dir);
    expect(warnings).toHaveLength(1);
    expect(warnings[0]).toMatch(/no DSN/);
    expect(server.requests).toHaveLength(0);
  });

  it("warns and lets the build succeed when the DSN is empty", async () => {
    const { dir } = await setup();
    const warnings = await build(vite({ dsn: "", release: "vite-1" }), dir);
    expect(warnings).toHaveLength(1);
  });

  it("fails the build when the upload is rejected", async () => {
    const { server, dir } = await setup(500);
    await expect(build(vite({ dsn: dsnFor(server.origin), release: "vite-1" }), dir)).rejects.toThrow(/upload failed \(500/);
  });

  it("does nothing with upload: false or outside a build", async () => {
    const { server, dir } = await setup();
    expect(await build(vite({ dsn: dsnFor(server.origin), release: "r", upload: false }), dir)).toEqual([]);
    expect(await build(vite({ dsn: dsnFor(server.origin), release: "r" }), dir, "serve")).toEqual([]);
    expect(server.requests).toHaveLength(0);
  });
});
