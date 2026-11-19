import { execFileSync } from "node:child_process";
import { promises as fs } from "node:fs";
import path from "node:path";

export interface UploadSourceMapsOptions {
  /** `<origin>/p/<project>@<token>`。缺失或为空时打一条警告并跳过上传（不算失败）。 */
  dsn?: string;
  /** 缺省取 `cwd` 下 git 的 7 位短 SHA。 */
  release?: string;
  /** 递归扫描 `.map` 文件的目录。 */
  dir: string;
  /** 上传成功后删除本地 `.map`。 */
  stripSourceMaps?: boolean;
  /** 推断 release 时执行 git 的目录，缺省 `process.cwd()`。 */
  cwd?: string;
  /** 警告出口，缺省 `console.warn`；Vite 插件传入 Rollup 的 `this.warn`。 */
  warn?: (message: string) => void;
}

export interface UploadSourceMapsResult {
  uploaded: number;
  /** 没有上传时的原因：未配置 DSN，或目录里没有 `.map`。 */
  skipped?: "no-dsn" | "no-source-maps";
  release?: string;
}

export interface SourceMapOutput { mapPath: string; filename: string }
type GitRunner = (cwd: string) => string;

export function inferRelease(explicit: string | undefined, cwd: string, runGit: GitRunner = defaultGitRunner): string {
  const configured = explicit?.trim();
  if (configured) return configured;
  const sha = runGit(cwd).trim();
  if (!sha) throw new Error("lazy-observe: unable to infer release from git; set release explicitly");
  return sha.slice(0, 7);
}

function defaultGitRunner(cwd: string): string {
  return execFileSync("git", ["rev-parse", "--short=7", "HEAD"], { cwd, encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] });
}

export async function collectSourceMaps(outputDir: string): Promise<SourceMapOutput[]> {
  const found: SourceMapOutput[] = [];
  async function visit(directory: string): Promise<void> {
    const entries = await fs.readdir(directory, { withFileTypes: true });
    await Promise.all(entries.map(async (entry) => {
      const absolute = path.join(directory, entry.name);
      if (entry.isDirectory()) await visit(absolute);
      else if (entry.isFile() && entry.name.endsWith(".map")) found.push({ mapPath: absolute, filename: entry.name.slice(0, -4) });
    }));
  }
  try { await visit(outputDir); }
  catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error; }
  found.sort((a, b) => a.mapPath.localeCompare(b.mapPath));
  const names = new Set<string>();
  for (const item of found) {
    if (names.has(item.filename)) throw new Error(`lazy-observe: duplicate minified filename ${item.filename}`);
    names.add(item.filename);
  }
  return found;
}

export function parseDsn(value: string): { origin: string; token: string } {
  let url: URL;
  try { url = new URL(value); }
  catch { throw new Error("lazy-observe: DSN must match <origin>/p/<project>@<token>"); }
  const match = url.pathname.match(/^\/p\/[^/@]+@([^/]+)\/?$/);
  if (!match || url.search || url.hash) throw new Error("lazy-observe: DSN must match <origin>/p/<project>@<token>");
  return { origin: url.origin, token: decodeURIComponent(match[1]) };
}

/**
 * 把 `dir` 下的 `.map` 以 multipart 一次性上传到 `POST /api/releases/:release/sourcemaps`。
 * 未配置 DSN 时只警告并返回；配置了 DSN 却上传失败时抛错（调用方据此让构建或命令失败）。
 */
export async function uploadSourceMaps(options: UploadSourceMapsOptions): Promise<UploadSourceMapsResult> {
  const warn = options.warn ?? ((message: string) => console.warn(message));
  const dsnValue = options.dsn?.trim();
  if (!dsnValue) {
    warn("lazy-observe: no DSN configured; skipping source map upload");
    return { uploaded: 0, skipped: "no-dsn" };
  }
  const dsn = parseDsn(dsnValue);
  const dir = path.resolve(options.dir);
  const stat = await fs.stat(dir).catch((error: NodeJS.ErrnoException) => {
    if (error.code === "ENOENT") return undefined;
    throw error;
  });
  if (!stat?.isDirectory()) throw new Error(`lazy-observe: source map directory not found: ${dir}`);
  const release = inferRelease(options.release, options.cwd ?? process.cwd());
  const files = await collectSourceMaps(dir);
  if (!files.length) {
    warn(`lazy-observe: no .map files found under ${dir}; nothing to upload`);
    return { uploaded: 0, skipped: "no-source-maps", release };
  }
  const form = new FormData();
  for (const file of files) form.append(file.filename, new Blob([await fs.readFile(file.mapPath)], { type: "application/json" }), `${file.filename}.map`);
  let response: Response;
  try {
    response = await fetch(`${dsn.origin}/api/releases/${encodeURIComponent(release)}/sourcemaps`, { method: "POST", headers: { Authorization: `Bearer ${dsn.token}` }, body: form });
  } catch (error) {
    // 网络层错误（undici 只报 "fetch failed"，原因在 cause 里）：带上 origin 便于定位，不带 token。
    const cause = (error as Error).cause;
    throw new Error(`lazy-observe: source map upload to ${dsn.origin} failed (${cause instanceof Error ? cause.message : (error as Error).message})`);
  }
  if (!response.ok) throw new Error(`lazy-observe: source map upload failed (${response.status} ${await response.text()})`);
  if (options.stripSourceMaps) await Promise.all(files.map((file) => fs.unlink(file.mapPath)));
  return { uploaded: files.length, release };
}
