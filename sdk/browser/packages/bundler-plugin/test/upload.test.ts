import { promises as fs } from "node:fs";
import path from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import { uploadSourceMaps } from "../src/index";
import { dsnFor, makeOutputDir, startServer } from "./helpers";

const cleanups: Array<() => Promise<void>> = [];
afterEach(async () => { await Promise.all(cleanups.splice(0).map((cleanup) => cleanup())); });

async function setup(status = 200) {
  const server = await startServer(status);
  const dir = await makeOutputDir();
  cleanups.push(server.close, () => fs.rm(dir, { recursive: true, force: true }));
  return { server, dir };
}

describe("uploadSourceMaps", () => {
  it("posts every map to the release endpoint with the DSN token", async () => {
    const { server, dir } = await setup();
    const result = await uploadSourceMaps({ dsn: dsnFor(server.origin), release: "rel/1", dir });
    expect(result).toEqual({ uploaded: 2, release: "rel/1" });
    expect(server.requests).toHaveLength(1);
    const [request] = server.requests;
    expect(request.method).toBe("POST");
    expect(request.url).toBe("/api/releases/rel%2F1/sourcemaps");
    expect(request.authorization).toBe("Bearer secret-token");
    expect(request.body).toContain('name="app.js"; filename="app.js.map"');
    expect(request.body).toContain('name="style.css"; filename="style.css.map"');
    expect(request.body).toContain('"sources":["app.ts"]');
    await expect(fs.stat(path.join(dir, "assets", "app.js.map"))).resolves.toBeTruthy();
  });

  it("deletes the local maps after a successful upload when stripSourceMaps is set", async () => {
    const { server, dir } = await setup();
    await uploadSourceMaps({ dsn: dsnFor(server.origin), release: "r", dir, stripSourceMaps: true });
    await expect(fs.stat(path.join(dir, "assets", "app.js.map"))).rejects.toMatchObject({ code: "ENOENT" });
    await expect(fs.stat(path.join(dir, "assets", "app.js"))).resolves.toBeTruthy();
  });

  it.each([undefined, "", "   "])("warns and skips when the DSN is %j", async (dsn) => {
    const { server, dir } = await setup();
    const warnings: string[] = [];
    const result = await uploadSourceMaps({ dsn, release: "r", dir, warn: (message) => warnings.push(message) });
    expect(result).toEqual({ uploaded: 0, skipped: "no-dsn" });
    expect(warnings).toHaveLength(1);
    expect(warnings[0]).toMatch(/no DSN/);
    expect(server.requests).toHaveLength(0);
  });

  it("throws on a rejected upload and keeps the local maps", async () => {
    const { server, dir } = await setup(403);
    await expect(uploadSourceMaps({ dsn: dsnFor(server.origin), release: "r", dir, stripSourceMaps: true }))
      .rejects.toThrow(/source map upload failed \(403/);
    await expect(fs.stat(path.join(dir, "assets", "app.js.map"))).resolves.toBeTruthy();
  });

  it("throws when the server is unreachable", async () => {
    const { server, dir } = await setup();
    const origin = server.origin;
    await server.close();
    cleanups.splice(cleanups.indexOf(server.close), 1);
    const error = await uploadSourceMaps({ dsn: dsnFor(origin), release: "r", dir }).catch((e: Error) => e);
    expect((error as Error).message).toContain(`lazy-observe: source map upload to ${origin} failed (`);
    expect((error as Error).message).not.toContain("secret-token");
  });

  it("throws when a DSN is set but malformed, without echoing it", async () => {
    const { dir } = await setup();
    const error = await uploadSourceMaps({ dsn: "not a dsn secret-token", release: "r", dir }).catch((e: Error) => e);
    expect(error).toBeInstanceOf(Error);
    expect((error as Error).message).toMatch(/DSN must match/);
    expect((error as Error).message).not.toContain("secret-token");
  });

  it("throws when the directory does not exist", async () => {
    const { server, dir } = await setup();
    await expect(uploadSourceMaps({ dsn: dsnFor(server.origin), release: "r", dir: path.join(dir, "missing") }))
      .rejects.toThrow(/directory not found/);
  });

  it("warns and uploads nothing when the directory has no maps", async () => {
    const { server, dir } = await setup();
    const empty = path.join(dir, "empty");
    await fs.mkdir(empty);
    const warnings: string[] = [];
    const result = await uploadSourceMaps({ dsn: dsnFor(server.origin), release: "r", dir: empty, warn: (message) => warnings.push(message) });
    expect(result).toMatchObject({ uploaded: 0, skipped: "no-source-maps" });
    expect(warnings[0]).toMatch(/no \.map files/);
    expect(server.requests).toHaveLength(0);
  });
});
