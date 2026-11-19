import path from "node:path";
import { inferRelease, uploadSourceMaps } from "./upload";

export interface LazyObservePluginOptions {
  /** 缺失或为空时构建照常成功，只警告并跳过上传。 */
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
      const value = inferRelease(options.release, configuredRoot);
      return { define: { __LAZY_OBSERVE_RELEASE__: JSON.stringify(value) } };
    },
    configResolved(config) {
      building = config.command === undefined || config.command === "build";
      root = config.root;
      outputDir = path.resolve(root, config.build.outDir);
      release = inferRelease(options.release, root);
    },
    async writeBundle() {
      if (!building || options.upload === false) return;
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
