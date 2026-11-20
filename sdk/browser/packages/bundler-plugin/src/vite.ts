import path from "node:path";
import { inferRelease, stripLocalSourceMaps, uploadSourceMaps } from "./upload";

export interface LazyObservePluginOptions {
  /** 缺失或为空时构建照常成功，只警告并跳过上传（此时也不要求能推断出 release）。 */
  dsn?: string;
  release?: string;
  upload?: boolean;
  stripSourceMaps?: boolean;
}

// 只声明插件用到的 Vite / Rollup 形状：包本身不依赖 vite 类型，结构上与 Vite 的 Plugin 兼容。
// 方法写成方法签名（双变），避免 UserConfig / ResolvedConfig / PluginContext 的完整类型被拉进来。
export interface LazyObserveVitePlugin {
  name: string;
  config(config: { root?: string }): { define: Record<string, string> };
  configResolved(config: { root: string; command?: string; build: { outDir: string } }): void;
  writeBundle(this: { warn(message: string): void }): Promise<void>;
}

export function vite(options: LazyObservePluginOptions): LazyObserveVitePlugin {
  let root = process.cwd();
  let outputDir = path.resolve(root, "dist");
  let release: string | undefined;
  let building = false;
  return {
    name: "lazy-observe-sourcemaps",
    config(config) {
      const configuredRoot = config.root ? path.resolve(config.root) : process.cwd();
      // 只是尽力推断：这里失败不能中断构建（没有 DSN 时根本不上传）。推断不出时注入 undefined，
      // 浏览器 SDK 的 release 本就可选；有 DSN 的 build 在 configResolved 里严格报错。
      const value = tryInferRelease(options.release, configuredRoot);
      return { define: { __LAZY_OBSERVE_RELEASE__: value === undefined ? "undefined" : JSON.stringify(value) } };
    },
    configResolved(config) {
      building = config.command === undefined || config.command === "build";
      root = config.root;
      outputDir = path.resolve(root, config.build.outDir);
      // 只有真的要上传（build、upload 未关、配置了 DSN）才必须有 release；其余情况不推断，不因 git 失败而报错。
      release = building && options.upload !== false && options.dsn?.trim() ? inferRelease(options.release, root) : undefined;
    },
    async writeBundle() {
      if (!building) return;
      if (options.upload === false) {
        if (options.stripSourceMaps) await stripLocalSourceMaps(outputDir);
        return;
      }
      await uploadSourceMaps({
        dsn: options.dsn,
        release,
        dir: outputDir,
        cwd: root,
        stripSourceMaps: options.stripSourceMaps,
        warn: (message) => this.warn(message)
      });
    }
  };
}

export default vite;

function tryInferRelease(explicit: string | undefined, cwd: string): string | undefined {
  try { return inferRelease(explicit, cwd); }
  catch { return undefined; }
}
