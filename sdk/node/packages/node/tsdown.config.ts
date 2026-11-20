import { defineConfig } from "tsdown";

export default defineConfig({
  entry: ["src/index.ts"],
  format: ["esm", "cjs"],
  platform: "node",
  // d.ts 由 oxc 按 isolatedDeclarations 生成：不依赖 TypeScript 的 JS API，TS 5/6/7 下产物一致；
  // 导出的类型必须显式标注，否则构建报错。
  dts: { generator: "oxc" },
  shims: true,
  sourcemap: true,
  clean: true,
  // package.json 的 main/module/types/exports 指向 .js/.cjs/.d.ts，扩展名由 type: module 决定，不用 .mjs。
  fixedExtension: false,
  // 源码是 ESM（隐式严格模式）；rolldown 默认不给 CJS 产物加 "use strict"，显式打开，与 tsup 时代一致；
  // esModule: true 保留 CJS 的 __esModule 标记（rolldown 默认只在有 default 导出时才加）。
  outputOptions: { strict: true, esModule: true },
});
