#!/usr/bin/env node
// SDK 构建产物快照：tsup → tsdown 迁移前后各跑一次，再用 diff 比对 JSON。
// 用法：node sdk-dist-snapshot.mjs <包目录> <输出.json>
// 包目录必须已安装依赖并已构建（dist/ 存在）。比对的是"公开 API 面"，不是 JS 字节：
//   1. dist 文件清单（相对路径 + 大小）；
//   2. package.json 的 main/module/types/exports 以及每个指向的文件是否存在；
//   3. ESM import / CJS require 在 node 下导出的名字与 typeof；
//   4. .d.ts / .d.cts 里导出的公开符号（名字、种类、类型文本），用 TypeScript checker 取；
//   5. 每个 JS 文件的 sourceMappingURL 与 .map 是否可解析；
//   6. JS / d.ts 里引用的裸模块说明符（检查 peer/external 没有被打进包里）。
import { execFileSync } from "node:child_process";
import { createRequire } from "node:module";
import fs from "node:fs";
import path from "node:path";

const [pkgDirArg, outArg] = process.argv.slice(2);
const pkgDir = path.resolve(pkgDirArg);
const pkg = JSON.parse(fs.readFileSync(path.join(pkgDir, "package.json"), "utf8"));
const dist = path.join(pkgDir, "dist");
// d.ts 比对用固定的 TypeScript 5.x JS API（TS 7 没有 JS API），与被测包构建时用的 TypeScript 版本无关。
// SNAPSHOT_TS_DIR 指向一个装了 typescript@5.9.x 的目录；未设置时退回包自己的 typescript。
const require = createRequire(path.join(process.env.SNAPSHOT_TS_DIR ?? pkgDir, "package.json"));
const ts = require("typescript");

const walk = (d) =>
  fs.readdirSync(d, { withFileTypes: true }).flatMap((e) => {
    const p = path.join(d, e.name);
    return e.isDirectory() ? walk(p) : [p];
  });
const files = walk(dist).map((p) => path.relative(pkgDir, p)).sort();

// 2. 入口指向
const targets = new Set();
const collect = (v) => {
  if (typeof v === "string") targets.add(v);
  else if (v && typeof v === "object") Object.values(v).forEach(collect);
};
[pkg.main, pkg.module, pkg.types].forEach(collect);
collect(pkg.exports);
const entries = {
  main: pkg.main,
  module: pkg.module,
  types: pkg.types,
  exports: pkg.exports,
  missingTargets: [...targets].filter((t) => !fs.existsSync(path.join(pkgDir, t))).sort(),
};

// 3. 运行时导出
const probe = (kind, file) => {
  const code =
    kind === "esm"
      ? `const m = await import(${JSON.stringify(file)}); console.log(JSON.stringify(Object.keys(m).sort().map(k => [k, typeof m[k]])));`
      : `const m = require(${JSON.stringify(file)}); console.log(JSON.stringify(Object.keys(m).sort().map(k => [k, typeof m[k]])));`;
  try {
    const args = kind === "esm" ? ["--input-type=module", "-e", code] : ["-e", code];
    return JSON.parse(execFileSync(process.execPath, args, { cwd: pkgDir, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }));
  } catch (e) {
    return { error: String(e.stderr || e.message).split("\n").slice(0, 3).join(" | ") };
  }
};
// 按 exports 的 import / require 条件各探测一次
const subpaths = {};
const exp = pkg.exports ?? { ".": { import: pkg.module, require: pkg.main } };
for (const [sub, cond] of Object.entries(exp)) {
  subpaths[sub] = {
    esm: cond.import ? probe("esm", path.join(pkgDir, cond.import)) : null,
    cjs: cond.require ? probe("cjs", path.join(pkgDir, cond.require)) : null,
  };
}

// 4. d.ts 公开符号
const dtsFiles = files.filter((f) => /\.d\.c?ts$/.test(f));
const dts = {};
for (const f of dtsFiles) {
  const abs = path.join(pkgDir, f);
  const program = ts.createProgram([abs], { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.NodeNext, moduleResolution: ts.ModuleResolutionKind.NodeNext, skipLibCheck: true, strict: true, noEmit: true, types: [] });
  const checker = program.getTypeChecker();
  const sf = program.getSourceFile(abs);
  const modSym = checker.getSymbolAtLocation(sf);
  const syms = modSym ? checker.getExportsOfModule(modSym) : [];
  dts[f] = syms
    .map((s) => {
      const target = s.flags & ts.SymbolFlags.Alias ? checker.getAliasedSymbol(s) : s;
      const decl = target.declarations?.[0];
      let text = "";
      try {
        const t = decl ? checker.getTypeOfSymbolAtLocation(target, decl) : undefined;
        text = t ? checker.typeToString(t, undefined, ts.TypeFormatFlags.NoTruncation | ts.TypeFormatFlags.InTypeAlias) : "";
        if (target.flags & (ts.SymbolFlags.Interface | ts.SymbolFlags.TypeAlias)) {
          text = checker.typeToString(checker.getDeclaredTypeOfSymbol(target), undefined, ts.TypeFormatFlags.NoTruncation | ts.TypeFormatFlags.InTypeAlias);
        }
      } catch {}
      return `${s.name} [${ts.SymbolFlags[target.flags & -target.flags] ?? target.flags}] ${text.replace(/\s+/g, " ")}`;
    })
    .sort();
}

// 5/6. source map 与外部依赖
const jsFiles = files.filter((f) => /\.(c|m)?js$/.test(f));
const maps = {};
const specs = {};
const specRe = /(?:from\s*|import\s*\(\s*|require\s*\(\s*|import\s+)["']([^"'./][^"']*)["']/g;
for (const f of [...jsFiles, ...dtsFiles]) {
  const src = fs.readFileSync(path.join(pkgDir, f), "utf8");
  specs[f] = [...new Set([...src.matchAll(specRe)].map((m) => m[1]))].sort();
  if (jsFiles.includes(f)) {
    const m = src.match(/\/\/# sourceMappingURL=(\S+)/);
    let info = { url: m?.[1] ?? null };
    if (m) {
      const mp = path.join(path.dirname(path.join(pkgDir, f)), m[1]);
      try {
        const j = JSON.parse(fs.readFileSync(mp, "utf8"));
        info = { ...info, version: j.version, sources: j.sources.length, hasContent: Array.isArray(j.sourcesContent) && j.sourcesContent.length > 0, file: j.file ?? null };
      } catch (e) {
        info.error = String(e.message);
      }
    }
    maps[f] = info;
  }
}

const sizes = Object.fromEntries(files.map((f) => [f, fs.statSync(path.join(pkgDir, f)).size]));
fs.writeFileSync(outArg, JSON.stringify({ pkg: pkg.name, files, sizes, entries, runtime: subpaths, dts, maps, specifiers: specs }, null, 2) + "\n");
console.log(`snapshot ${pkg.name} -> ${outArg} (${files.length} files)`);
