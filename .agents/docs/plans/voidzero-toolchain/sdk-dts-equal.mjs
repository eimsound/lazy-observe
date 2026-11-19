#!/usr/bin/env node
// 两份 d.ts 的结构等价检查：对旧 d.ts 导出的每个符号，生成 "旧类型与新类型互相可赋值" 的断言，再用 tsc 编译。
// 比 sdk-dist-snapshot.mjs 里的类型文本更严格（接口成员、类、重载都会比）。
// 用法：SNAPSHOT_TS_DIR=<装了 typescript 5.x 的目录> node sdk-dts-equal.mjs <旧 .d.ts> <新 .d.ts>
import { createRequire } from "node:module";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const [oldDts, newDts] = process.argv.slice(2).map((p) => path.resolve(p));
const require = createRequire(path.join(process.env.SNAPSHOT_TS_DIR, "package.json"));
const ts = require("typescript");
const opts = { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.NodeNext, moduleResolution: ts.ModuleResolutionKind.NodeNext, skipLibCheck: true, strict: true, noEmit: true, types: [] };

const exportsOf = (file) => {
  const program = ts.createProgram([file], opts);
  const checker = program.getTypeChecker();
  return checker.getExportsOfModule(checker.getSymbolAtLocation(program.getSourceFile(file))).map((s) => {
    const t = s.flags & ts.SymbolFlags.Alias ? checker.getAliasedSymbol(s) : s;
    const isType = !!(t.flags & ts.SymbolFlags.Type);
    let arity = 0;
    if (isType) {
      const dt = checker.getDeclaredTypeOfSymbol(t);
      arity = (dt.aliasTypeArguments ?? dt.typeParameters ?? dt.target?.typeParameters ?? []).length;
    }
    // 带 private 成员的 class 是名义类型，两份 d.ts 里的同名 class 互不可赋值；改比较声明文本（去掉 export/declare 与空白）。
    const isClass = !!(t.flags & ts.SymbolFlags.Class);
    const text = isClass ? t.declarations[0].getText().replace(/\b(export|declare)\s+/g, "").replace(/\s+/g, " ") : undefined;
    return { name: s.name, value: !!(t.flags & ts.SymbolFlags.Value), type: isType, arity, isClass, text };
  });
};
const oldExports = exportsOf(oldDts);
const newExports = exportsOf(newDts);
const newNames = new Set(newExports.map((e) => e.name));
const classMismatch = oldExports.filter((e) => e.isClass).filter((e) => newExports.find((n) => n.name === e.name)?.text !== e.text).map((e) => e.name);
if (classMismatch.length) console.log("class declaration text differs:", classMismatch);
const missing = oldExports.filter((e) => !newNames.has(e.name)).map((e) => e.name);
const q = (s) => JSON.stringify(s);
const lines = [
  `import type * as O from ${q(oldDts.replace(/\.d\.(c?)ts$/, ".d.$1ts").replace(/\.ts$/, ""))};`,
  `import type * as N from ${q(newDts.replace(/\.ts$/, ""))};`,
  "type Eq<A, B> = [A] extends [B] ? ([B] extends [A] ? true : false) : false;",
];
for (const e of oldExports) {
  if (e.isClass) continue;
  const id = e.name === "default" ? "default" : e.name;
  if (e.value) lines.push(`export const v_${e.name}: Eq<typeof O.${id}, typeof N.${id}> = true;`);
  if (e.type) {
    const args = e.arity ? `<${Array(e.arity).fill("any").join(", ")}>` : "";
    lines.push(`export const t_${e.name}: Eq<O.${id}${args}, N.${id}${args}> = true;`);
  }
}
const dir = fs.mkdtempSync(path.join(os.homedir(), ".cache", "dts-equal-"));
const probe = path.join(dir, "probe.ts");
fs.writeFileSync(probe, lines.join("\n") + "\n");
const program = ts.createProgram([probe], { ...opts, allowImportingTsExtensions: true });
const diags = ts.getPreEmitDiagnostics(program);
for (const d of diags) {
  const where = d.file ? lines[d.file.getLineAndCharacterOfPosition(d.start).line] : "";
  console.log(ts.flattenDiagnosticMessageText(d.messageText, "\n"), "<=", where);
}
fs.rmSync(dir, { recursive: true });
console.log(`exports old=${oldExports.length} missingInNew=${JSON.stringify(missing)} assertions=${lines.length - 3} classesComparedByText=${oldExports.filter((e) => e.isClass).length} diagnostics=${diags.length}`);
process.exit(diags.length || missing.length || classMismatch.length ? 1 : 0);
