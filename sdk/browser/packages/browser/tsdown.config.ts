import { defineConfig } from "tsdown";

export default defineConfig({
  entry: ["src/index.ts"],
  format: ["esm", "cjs"],
  // cjs 格式固定面向 Node，platform: "browser" 对它无效，所以用 neutral。
  platform: "neutral",
  // d.ts 由 oxc 按 isolatedDeclarations 生成：不依赖 TypeScript 的 JS API，TS 5/6/7 下产物一致；
  // 导出的类型必须显式标注，否则构建报错。
  dts: { generator: "oxc" },
  sourcemap: true,
  clean: true,
  treeshake: true,
  target: "es2022",
  // package.json 的 main/module/types/exports 指向 .js/.cjs/.d.ts，扩展名由 type: module 决定，不用 .mjs。
  fixedExtension: false,
  // 源码是 ESM（隐式严格模式）；rolldown 默认不给 CJS 产物加 "use strict"，显式打开，与 tsup 时代一致。
  outputOptions: { strict: true },
  deps: {
    // 运行时依赖留给使用方安装，不能打进包里。
    neverBundle: ["rrweb", "web-vitals"],
    // 白名单为空：node_modules 里任何包被打进产物都直接报错。
    onlyBundle: [],
  },
});
