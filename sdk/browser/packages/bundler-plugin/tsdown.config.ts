import { defineConfig } from "tsdown";

export default defineConfig({
  entry: { index: "src/index.ts", vite: "src/vite.ts", cli: "src/cli.ts" },
  // 只发 ESM：包是零运行时依赖，Node >=22.15 的 require() 能直接加载 ESM（见 test/smoke.test.ts）。
  format: ["esm"],
  platform: "node",
  // d.ts 由 oxc 按 isolatedDeclarations 生成：不依赖 TypeScript 的 JS API，TS 5/6/7 下产物一致；
  // 导出的类型必须显式标注，否则构建报错。
  dts: { generator: "oxc" },
  sourcemap: true,
  clean: true,
  target: "node22",
  // package.json 的 exports/bin 指向 .js/.d.ts，扩展名由 type: module 决定，不用 .mjs。
  fixedExtension: false,
  deps: {
    // 白名单为空：node_modules 里任何包被打进产物都直接报错（零运行时依赖）。
    onlyBundle: [],
  },
});
