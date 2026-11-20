import { execFileSync } from "node:child_process";
import { promises as fs } from "node:fs";
import path from "node:path";

const SOURCE_MAP_COMPONENT = /^[A-Za-z0-9._-]+$/;
const DEFAULT_UPLOAD_TIMEOUT_MS = 30_000;
const CONTROL_OR_WHITESPACE = /[\u0000-\u001f\u007f\s]/;
const SOURCE_MAP_TOKEN_PREFIX = "los_";
const EXPECTED_DSN = "lazy-observe: DSN must match http(s)://<host>/p/<project>@<token>";

export interface ParsedUploadDsn {
  origin: string;
  project: string;
  token: string;
}

export interface UploadSourceMapsOptions {
  /** `<origin>/p/<project>@<token>`。缺失或为空时打一条警告并跳过上传（不算失败）。 */
  dsn?: string;
  /** 缺省取 `cwd` 下 git 的 7 位短 SHA。 */
  release?: string;
  /** 递归扫描 `.map` 文件的目录。 */
  dir: string;
  /** 删除本地 `.map`（无 DSN 时同样执行剥离，避免线上暴露源码映射；有 DSN 但上传失败时保留以便排查）。 */
  stripSourceMaps?: boolean;
  /** 推断 release 时执行 git 的目录，缺省 `process.cwd()`。 */
  cwd?: string;
  /** 警告出口，缺省 `console.warn`；Vite 插件传入 Rollup 的 `this.warn`。 */
  warn?: (message: string) => void;
  /** 上传请求超时时间（毫秒），默认 30 秒。 */
  timeoutMs?: number;
}

export interface UploadSourceMapsResult {
  uploaded: number;
  /** 没有上传时的原因：未配置 DSN，或目录里没有 `.map`。 */
  skipped?: "no-dsn" | "no-source-maps";
  release?: string;
}

export interface SourceMapOutput { mapPath: string; filename: string }
type GitRunner = (cwd: string) => string;

/**
 * 规范化源映射对应的压缩文件名，确保满足服务端 `^[A-Za-z0-9._-]+$` 校验
 * （例如 Next.js 动态路由 chunk `[id]-abc.js`、`[...slug].js`）。
 * 原本已合规的文件名保持不变；不合规字符按十六进制 `_XX_` 确定性编码避免冲突。
 */
export function normalizeSourceMapFilename(raw: string): string {
  const trimmed = raw.trim();
  if (SOURCE_MAP_COMPONENT.test(trimmed) && trimmed !== "." && trimmed !== "..") {
    return trimmed;
  }
  const encoded = trimmed.replace(
    /[^A-Za-z0-9._-]/g,
    (ch) => `_${ch.charCodeAt(0).toString(16).toUpperCase()}_`,
  );
  if (!encoded || encoded === "." || encoded === "..") {
    throw new Error(`lazy-observe: invalid source map filename ${JSON.stringify(raw)}`);
  }
  return encoded;
}

export function validateRelease(release: string): string {
  const trimmed = release.trim();
  if (!trimmed || trimmed === "." || trimmed === ".." || !SOURCE_MAP_COMPONENT.test(trimmed)) {
    throw new Error(
      `lazy-observe: invalid release ${JSON.stringify(release)}; release must match /^[A-Za-z0-9._-]+$/`,
    );
  }
  return trimmed;
}

export function inferRelease(explicit: string | undefined, cwd: string, runGit: GitRunner = defaultGitRunner): string {
  const configured = explicit?.trim();
  if (configured) return validateRelease(configured);
  let sha = "";
  try { sha = runGit(cwd).trim(); }
  catch (error) {
    // 没有 .git（例如 Docker 构建排除了它）或没装 git：给出可操作的报错，而不是 execFileSync 的 "Command failed"。
    throw new Error("lazy-observe: unable to infer release from git; set release explicitly", { cause: error });
  }
  if (!sha) throw new Error("lazy-observe: unable to infer release from git; set release explicitly");
  return validateRelease(sha.slice(0, 7));
}

function defaultGitRunner(cwd: string): string {
  return execFileSync("git", ["rev-parse", "--short=7", "HEAD"], { cwd, encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] });
}

async function collectRawMapPaths(outputDir: string): Promise<string[]> {
  const paths: string[] = [];
  async function visit(directory: string): Promise<void> {
    const entries = await fs.readdir(directory, { withFileTypes: true });
    await Promise.all(entries.map(async (entry) => {
      const absolute = path.join(directory, entry.name);
      if (entry.isDirectory()) await visit(absolute);
      else if (entry.isFile() && entry.name.endsWith(".map")) paths.push(absolute);
    }));
  }
  try { await visit(outputDir); }
  catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error; }
  paths.sort((a, b) => a.localeCompare(b));
  return paths;
}

export async function stripLocalSourceMaps(outputDir: string): Promise<number> {
  const mapPaths = await collectRawMapPaths(outputDir);
  await Promise.all(mapPaths.map((mapPath) => fs.unlink(mapPath)));
  return mapPaths.length;
}

export async function collectSourceMaps(outputDir: string): Promise<SourceMapOutput[]> {
  const mapPaths = await collectRawMapPaths(outputDir);
  const found: SourceMapOutput[] = mapPaths.map((mapPath) => ({
    mapPath,
    filename: normalizeSourceMapFilename(path.basename(mapPath).slice(0, -4)),
  }));
  const names = new Set<string>();
  for (const item of found) {
    if (names.has(item.filename)) throw new Error(`lazy-observe: duplicate minified filename ${item.filename}`);
    names.add(item.filename);
  }
  return found;
}

export function parseDsn(value: string): ParsedUploadDsn {
  if (typeof value !== "string" || !value.trim()) {
    throw new Error(EXPECTED_DSN);
  }
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    // Do not attach cause or echo the raw DSN so los_ tokens never leak into CI logs.
    throw new Error(EXPECTED_DSN);
  }
  if (
    (url.protocol !== "http:" && url.protocol !== "https:")
    || !url.host
    || url.username
    || url.password
    || url.search
    || url.hash
  ) {
    throw new Error(EXPECTED_DSN);
  }
  const match = url.pathname.match(/^\/p\/([^/@]+)@([^/@]+)\/?$/);
  if (!match) {
    throw new Error(EXPECTED_DSN);
  }
  let project: string;
  let token: string;
  try {
    project = decodeURIComponent(match[1]);
    token = decodeURIComponent(match[2]);
  } catch {
    throw new Error(EXPECTED_DSN);
  }
  if (!project.trim() || !token.trim() || CONTROL_OR_WHITESPACE.test(token)) {
    throw new Error(EXPECTED_DSN);
  }
  if (!token.startsWith(SOURCE_MAP_TOKEN_PREFIX) || token.length <= SOURCE_MAP_TOKEN_PREFIX.length) {
    throw new Error(
      "lazy-observe: source map upload requires a source map upload token (los_...); project API tokens (lo_...) are rejected",
    );
  }
  return { origin: url.origin, project, token };
}

export function redactDsn(value: string): string {
  try {
    const parsed = parseDsn(value);
    return `${parsed.origin}/p/${encodeURIComponent(parsed.project)}@[redacted]`;
  } catch {
    return "[invalid DSN]";
  }
}

/**
 * 把 `dir` 下的 `.map` 以 multipart 一次性上传到 `POST /api/releases/:release/sourcemaps`。
 * 未配置 DSN 时只警告并跳过上传（若配置了 `stripSourceMaps: true` 仍剥离本地 `.map`）；
 * 配置了 DSN 却上传失败时抛错并保留本地 `.map`（调用方据此让构建或命令失败）。
 */
export async function uploadSourceMaps(options: UploadSourceMapsOptions): Promise<UploadSourceMapsResult> {
  const warn = options.warn ?? ((message: string) => console.warn(message));
  const dsnValue = options.dsn?.trim();
  const dir = path.resolve(options.dir);
  if (!dsnValue) {
    warn("lazy-observe: no DSN configured; skipping source map upload");
    if (options.stripSourceMaps) await stripLocalSourceMaps(dir);
    return { uploaded: 0, skipped: "no-dsn" };
  }
  const dsn = parseDsn(dsnValue);
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
  const timeoutMs = options.timeoutMs ?? DEFAULT_UPLOAD_TIMEOUT_MS;
  let response: Response;
  try {
    response = await fetch(`${dsn.origin}/api/releases/${encodeURIComponent(release)}/sourcemaps`, {
      method: "POST",
      headers: { Authorization: `Bearer ${dsn.token}` },
      body: form,
      signal: AbortSignal.timeout(timeoutMs),
    });
  } catch (error) {
    // 网络层错误（undici 只报 "fetch failed"，原因在 cause 里）：带上 origin 便于定位，不带 token。
    const cause = (error as Error).cause;
    throw new Error(`lazy-observe: source map upload to ${dsn.origin} failed (${cause instanceof Error ? cause.message : (error as Error).message})`);
  }
  if (!response.ok) throw new Error(`lazy-observe: source map upload failed (${response.status} ${await response.text()})`);
  if (options.stripSourceMaps) await Promise.all(files.map((file) => fs.unlink(file.mapPath)));
  return { uploaded: files.length, release };
}
