import { promises as fs } from "node:fs";
import path from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import { parseDsn, redactDsn, uploadSourceMaps } from "../src/index";
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
    const result = await uploadSourceMaps({ dsn: dsnFor(server.origin), release: "rel-1.0", dir });
    expect(result).toEqual({ uploaded: 2, release: "rel-1.0" });
    expect(server.requests).toHaveLength(1);
    const [request] = server.requests;
    expect(request.method).toBe("POST");
    expect(request.url).toBe("/api/releases/rel-1.0/sourcemaps");
    expect(request.authorization).toBe("Bearer los_secret-token");
    expect(request.body).toContain('name="app.js"; filename="app.js.map"');
    expect(request.body).toContain('name="style.css"; filename="style.css.map"');
    expect(request.body).toContain('"sources":["app.ts"]');
    await expect(fs.stat(path.join(dir, "assets", "app.js.map"))).resolves.toBeTruthy();
  });

  it("normalizes Next.js dynamic route chunk names like [id] to satisfy server regex", async () => {
    const { server, dir } = await setup();
    await fs.writeFile(path.join(dir, "assets", "[id]-abc123.js.map"), '{"version":3,"sources":["[id].tsx"]}');
    const result = await uploadSourceMaps({ dsn: dsnFor(server.origin), release: "next-1", dir });
    expect(result).toEqual({ uploaded: 3, release: "next-1" });
    const [request] = server.requests;
    expect(request.body).toContain('name="_5B_id_5D_-abc123.js"; filename="_5B_id_5D_-abc123.js.map"');
    expect(request.body).not.toContain('name="[id]-abc123.js"');
  });

  it("rejects releases containing slashes or invalid characters before uploading", async () => {
    const { server, dir } = await setup();
    await expect(uploadSourceMaps({ dsn: dsnFor(server.origin), release: "rel/1", dir }))
      .rejects.toThrow(/invalid release/);
    expect(server.requests).toHaveLength(0);
  });

  it("strips local .map files even when no DSN is configured if stripSourceMaps is true", async () => {
    const { server, dir } = await setup();
    const warnings: string[] = [];
    const result = await uploadSourceMaps({
      dsn: undefined,
      release: "r",
      dir,
      stripSourceMaps: true,
      warn: (message) => warnings.push(message),
    });
    expect(result).toEqual({ uploaded: 0, skipped: "no-dsn" });
    expect(warnings[0]).toMatch(/no DSN/);
    expect(server.requests).toHaveLength(0);
    await expect(fs.stat(path.join(dir, "assets", "app.js.map"))).rejects.toMatchObject({ code: "ENOENT" });
    await expect(fs.stat(path.join(dir, "assets", "app.js"))).resolves.toBeTruthy();
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
    const error = await uploadSourceMaps({ dsn: "not a dsn los_secret-token", release: "r", dir }).catch((e: Error) => e);
    expect(error).toBeInstanceOf(Error);
    expect((error as Error).message).toMatch(/DSN must match/);
    expect((error as Error).message).not.toContain("secret-token");
  });

  it.each([
    "https://observe.example/p/default@lo_project_secret_123",
    "https://observe.example/p/default@lou_user_secret_123",
    "https://observe.example/p/default@plain_secret_123",
    "https://observe.example/p/default@los_",
  ])("rejects non-los_ DSN %s before making any HTTP request and without leaking the token", async (badDsn) => {
    const { server, dir } = await setup();
    const error = await uploadSourceMaps({ dsn: badDsn, release: "r", dir }).catch((e: Error) => e);
    expect(error).toBeInstanceOf(Error);
    expect((error as Error).message).toMatch(/source map upload token \(los_/);
    expect((error as Error).message).not.toContain("secret_123");
    expect((error as Error & { cause?: unknown }).cause).toBeUndefined();
    expect(server.requests).toHaveLength(0);
  });

  it("enforces aligned DSN validation and redaction in parseDsn / redactDsn", () => {
    expect(parseDsn("https://observe.example/p/my-app@los_abc123")).toEqual({
      origin: "https://observe.example",
      project: "my-app",
      token: "los_abc123",
    });
    expect(redactDsn("https://observe.example/p/my-app@los_abc123")).toBe(
      "https://observe.example/p/my-app@[redacted]",
    );
    expect(redactDsn("bad@los_abc123")).toBe("[invalid DSN]");
    for (const bad of [
      "ftp://observe.example/p/app@los_abc",
      "https://user:pw@observe.example/p/app@los_abc",
      "https://observe.example/p/app@los_abc?q=1",
      "https://observe.example/p/app@los_abc#frag",
      "https://observe.example/p/app@los_a@b",
      "https://observe.example/p/app@los_%ZZ",
    ]) {
      expect(() => parseDsn(bad)).toThrow(/DSN must match/);
    }
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

  it("aborts upload when timeoutMs elapses", async () => {
    const { dir } = await setup();
    const http = await import("node:http");
    const sockets = new Set<import("node:net").Socket>();
    const slow = http.createServer((_req, _res) => { /* intentionally never responds */ });
    slow.on("connection", (socket) => { sockets.add(socket); socket.on("close", () => sockets.delete(socket)); });
    await new Promise<void>((resolve) => slow.listen(0, "127.0.0.1", resolve));
    const address = slow.address() as import("node:net").AddressInfo;
    cleanups.push(() => new Promise<void>((resolve) => {
      for (const socket of sockets) socket.destroy();
      slow.close(() => resolve());
    }));
    await expect(uploadSourceMaps({
      dsn: dsnFor(`http://127.0.0.1:${address.port}`),
      release: "r-timeout",
      dir,
      timeoutMs: 50,
    })).rejects.toThrow(/source map upload to .* failed/);
  });
});
