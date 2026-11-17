import { execFileSync } from "node:child_process";
import { promises as fs } from "node:fs";
import path from "node:path";
import { createUnplugin } from "unplugin";

export interface LazyObservePluginOptions {
  dsn: string;
  release?: string;
  upload?: boolean;
  stripSourceMaps?: boolean;
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

async function uploadSourceMaps(options: LazyObservePluginOptions, release: string, outputDir: string): Promise<void> {
  if (options.upload === false) return;
  const files = await collectSourceMaps(outputDir);
  if (!files.length) return;
  const dsn = parseDsn(options.dsn);
  const form = new FormData();
  for (const file of files) form.append(file.filename, new Blob([await fs.readFile(file.mapPath)], { type: "application/json" }), `${file.filename}.map`);
  const response = await fetch(`${dsn.origin}/api/releases/${encodeURIComponent(release)}/sourcemaps`, { method: "POST", headers: { Authorization: `Bearer ${dsn.token}` }, body: form });
  if (!response.ok) throw new Error(`lazy-observe: source map upload failed (${response.status} ${await response.text()})`);
  if (options.stripSourceMaps) await Promise.all(files.map((file) => fs.unlink(file.mapPath)));
}

function parseDsn(value: string): { origin: string; token: string } {
  const url = new URL(value);
  const match = url.pathname.match(/^\/p\/[^/@]+@([^/]+)\/?$/);
  if (!match || url.search || url.hash) throw new Error("lazy-observe: DSN must match <origin>/p/<project>@<token>");
  return { origin: url.origin, token: decodeURIComponent(match[1]) };
}

const unplugin = createUnplugin<LazyObservePluginOptions>((options) => {
  let root = process.cwd();
  let outputDir = path.resolve(root, "dist");
  let release: string;
  let viteBuild = false;
  const resolvedRelease = () => release ??= inferRelease(options.release, root);
  return {
    name: "lazy-observe-sourcemaps",
    vite: {
      config(config) {
        const configuredRoot = config.root ? path.resolve(config.root) : process.cwd();
        const value = inferRelease(options.release, configuredRoot);
        return { define: { __LAZY_OBSERVE_RELEASE__: JSON.stringify(value) } };
      },
      configResolved(config) {
        viteBuild = true;
        root = config.root;
        outputDir = path.resolve(root, config.build.outDir);
        release = inferRelease(options.release, root);
      }
    },
    async writeBundle() {
      if (viteBuild) await uploadSourceMaps(options, resolvedRelease(), outputDir);
    },
    webpack(compiler) {
      root = compiler.context;
      outputDir = compiler.options.output.path ?? path.resolve(root, "dist");
      release = inferRelease(options.release, root);
      compiler.hooks.afterEmit.tapPromise("lazy-observe-sourcemaps", () => uploadSourceMaps(options, release, outputDir));
    }
  };
});

export const vite = unplugin.vite;
export const webpack = unplugin.webpack;
export default vite;
