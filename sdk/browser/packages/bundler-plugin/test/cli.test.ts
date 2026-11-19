import { execFile } from "node:child_process";
import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, describe, expect, it } from "vitest";
import { dsnFor, makeNonGitDir, makeOutputDir, startServer } from "./helpers";

// 测的是构建产物 dist/cli.js（mise 的 sdk-browser 任务先 build 再 test）。
const cli = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../dist/cli.js");

interface Run { code: number; stdout: string; stderr: string }
function run(args: string[], env: Record<string, string> = {}, cwd?: string): Promise<Run> {
  return new Promise((resolve) => {
    // 不继承外层的 LO_DSN / LO_RELEASE，避免本机环境影响断言。
    const { LO_DSN: _dsn, LO_RELEASE: _release, ...base } = process.env;
    execFile(process.execPath, [cli, ...args], { env: { ...base, ...env }, cwd }, (error, stdout, stderr) => {
      const code = error ? (typeof error.code === "number" ? error.code : 1) : 0;
      resolve({ code, stdout, stderr });
    });
  });
}

const cleanups: Array<() => Promise<void>> = [];
afterEach(async () => { await Promise.all(cleanups.splice(0).map((cleanup) => cleanup())); });

async function setup(status = 200) {
  const server = await startServer(status);
  const dir = await makeOutputDir();
  cleanups.push(server.close, () => fs.rm(dir, { recursive: true, force: true }));
  return { server, dir };
}

describe("lazy-observe-sourcemaps upload", () => {
  it("uploads a directory with --dsn and --release", async () => {
    const { server, dir } = await setup();
    const result = await run(["upload", "--dir", dir, "--dsn", dsnFor(server.origin), "--release", "next-1"]);
    expect(result).toMatchObject({ code: 0, stderr: "" });
    expect(result.stdout).toContain("uploaded 2 source map(s) for release next-1");
    expect(server.requests).toHaveLength(1);
    expect(server.requests[0]).toMatchObject({ method: "POST", url: "/api/releases/next-1/sourcemaps", authorization: "Bearer secret-token" });
    expect(server.requests[0].body).toContain('filename="app.js.map"');
  });

  it("reads the DSN and release from LO_DSN and LO_RELEASE and strips with --strip", async () => {
    const { server, dir } = await setup();
    const result = await run(["upload", "--dir", dir, "--strip"], { LO_DSN: dsnFor(server.origin), LO_RELEASE: "env-1" });
    expect(result.code).toBe(0);
    expect(server.requests[0].url).toBe("/api/releases/env-1/sourcemaps");
    await expect(fs.stat(path.join(dir, "assets", "app.js.map"))).rejects.toMatchObject({ code: "ENOENT" });
  });

  it("defaults the release to the seven-character git SHA", async () => {
    const { server, dir } = await setup();
    const result = await run(["upload", "--dir", dir, "--dsn", dsnFor(server.origin)]);
    expect(result.code).toBe(0);
    expect(server.requests[0].url).toMatch(/^\/api\/releases\/[0-9a-f]{7}\/sourcemaps$/);
  });

  it("warns, uploads nothing and exits 0 without a DSN", async () => {
    const { server, dir } = await setup();
    const result = await run(["upload", "--dir", dir]);
    expect(result.code).toBe(0);
    expect(result.stderr).toMatch(/no DSN configured/);
    expect(server.requests).toHaveLength(0);
  });

  describe("outside a git checkout without a release", () => {
    async function noGitDir() {
      const cwd = await makeNonGitDir();
      cleanups.push(() => fs.rm(cwd, { recursive: true, force: true }));
      return cwd;
    }

    it("still warns and exits 0 without a DSN", async () => {
      const { dir } = await setup();
      const result = await run(["upload", "--dir", dir], {}, await noGitDir());
      expect(result.code).toBe(0);
      expect(result.stderr).toMatch(/no DSN configured/);
    });

    it("exits 1 with a clear message when a DSN is set", async () => {
      const { server, dir } = await setup();
      const result = await run(["upload", "--dir", dir, "--dsn", dsnFor(server.origin)], {}, await noGitDir());
      expect(result.code).toBe(1);
      expect(result.stderr).toMatch(/unable to infer release from git; set release explicitly/);
      expect(result.stderr).not.toContain("secret-token");
      expect(server.requests).toHaveLength(0);
    });
  });

  it("treats an empty LO_DSN like a missing one", async () => {
    const { dir } = await setup();
    const result = await run(["upload", "--dir", dir], { LO_DSN: "" });
    expect(result.code).toBe(0);
    expect(result.stderr).toMatch(/no DSN configured/);
  });

  it("exits 1 when the upload is rejected", async () => {
    const { server, dir } = await setup(401);
    const result = await run(["upload", "--dir", dir, "--dsn", dsnFor(server.origin), "--release", "r"]);
    expect(result.code).toBe(1);
    expect(result.stderr).toMatch(/source map upload failed \(401/);
    expect(result.stderr).not.toContain("secret-token");
  });

  it("exits 1 when the directory does not exist", async () => {
    const { server, dir } = await setup();
    const result = await run(["upload", "--dir", path.join(dir, "missing"), "--dsn", dsnFor(server.origin), "--release", "r"]);
    expect(result.code).toBe(1);
    expect(result.stderr).toMatch(/directory not found/);
  });

  it("exits 2 on usage errors", async () => {
    expect((await run(["upload"])).code).toBe(2);
    expect((await run(["bogus", "--dir", "x"])).code).toBe(2);
    expect((await run(["upload", "--dir", "x", "--nope"])).code).toBe(2);
  });

  it("prints usage for --help", async () => {
    const result = await run(["--help"]);
    expect(result.code).toBe(0);
    expect(result.stdout).toContain("Usage: lazy-observe-sourcemaps upload");
  });
});
