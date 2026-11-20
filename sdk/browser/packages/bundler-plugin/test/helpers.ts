import { promises as fs } from "node:fs";
import http from "node:http";
import type { AddressInfo } from "node:net";
import os from "node:os";
import path from "node:path";

export interface RecordedRequest { method?: string; url?: string; authorization?: string; body: string }

/** 本地假平台：记录每个请求，按 `status` 回应。 */
export async function startServer(status = 200): Promise<{ origin: string; requests: RecordedRequest[]; close(): Promise<void> }> {
  const requests: RecordedRequest[] = [];
  const server = http.createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (chunk: Buffer) => chunks.push(chunk));
    req.on("end", () => {
      requests.push({ method: req.method, url: req.url, authorization: req.headers.authorization, body: Buffer.concat(chunks).toString("utf8") });
      res.writeHead(status, { "content-type": "application/json" });
      res.end(status === 200 ? '{"ok":true}' : '{"error":"rejected"}');
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address() as AddressInfo;
  return {
    origin: `http://127.0.0.1:${port}`,
    requests,
    close: () => new Promise<void>((resolve, reject) => server.close((error) => (error ? reject(error) : resolve())))
  };
}

/** 在 `~/.cache` 之外的系统临时目录建一棵 `assets/app.js(.map)` 与 `style.css.map` 的输出目录。 */
export async function makeOutputDir(): Promise<string> {
  const directory = await fs.mkdtemp(path.join(os.tmpdir(), "lazy-observe-upload-"));
  await fs.mkdir(path.join(directory, "assets"));
  await Promise.all([
    fs.writeFile(path.join(directory, "assets", "app.js"), ""),
    fs.writeFile(path.join(directory, "assets", "app.js.map"), '{"version":3,"sources":["app.ts"]}'),
    fs.writeFile(path.join(directory, "style.css.map"), '{"version":3,"sources":["style.css"]}')
  ]);
  return directory;
}

export function dsnFor(origin: string): string { return `${origin}/p/default@los_secret-token`; }

/** 一个不在任何 git 仓库里的空目录（模拟 Docker 构建排除了 .git），用来让 git 推断 release 失败。 */
export async function makeNonGitDir(): Promise<string> {
  return fs.mkdtemp(path.join(os.tmpdir(), "lazy-observe-nogit-"));
}
