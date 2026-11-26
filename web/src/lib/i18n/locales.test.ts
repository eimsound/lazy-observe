import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "vitest";

import { en } from "./locales/en.ts";
import { zh } from "./locales/zh.ts";

// Static `t("ns.key")` / `t('ns.key', …)` calls; template literals with `${…}` are dynamic and skipped.
const T_CALL = /\bt\(\s*(["'`])([A-Za-z0-9_.-]+)\1\s*[,)]/g;
const SRC_DIR = fileURLToPath(new URL("../..", import.meta.url));
const LOCALES = { zh, en } as const;

function sourceFiles(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) return sourceFiles(path);
    if (!/\.tsx?$/.test(entry.name) || /\.test\.tsx?$/.test(entry.name)) return [];
    return [path];
  });
}

function literalKeys(file: string): string[] {
  return [...readFileSync(file, "utf8").matchAll(T_CALL)].map((match) => match[2]);
}

function resolves(dict: unknown, key: string): boolean {
  let node = dict;
  for (const part of key.split(".")) {
    if (typeof node !== "object" || node === null || !Object.hasOwn(node, part)) return false;
    node = (node as Record<string, unknown>)[part];
  }
  return typeof node === "string";
}

function missingKeys(files: string[]): string[] {
  const missing: string[] = [];
  for (const file of files) {
    for (const key of literalKeys(file)) {
      for (const [lang, dict] of Object.entries(LOCALES)) {
        if (!resolves(dict, key)) missing.push(`${relative(SRC_DIR, file)}: ${lang} is missing "${key}"`);
      }
    }
  }
  return missing;
}

test("command palette and trace search keys exist in every locale", () => {
  const files = ["components/layout/command-palette.tsx", "routes/traces-page.tsx"].map((file) => join(SRC_DIR, file));
  const keys = new Set(files.flatMap(literalKeys));
  for (const key of [
    "palette.placeholder",
    "palette.empty",
    "palette.jumpTrace",
    "traces.minDuration",
    "traces.minDurationPlaceholder",
  ]) {
    assert.ok(keys.has(key), `expected a literal t("${key}") call`);
  }
  assert.deepEqual(missingKeys(files), []);
});

test("palette.jumpTrace interpolates the trace id", () => {
  for (const dict of Object.values(LOCALES)) assert.match(dict.palette.jumpTrace, /\{\{id\}\}/);
});

test("every static t() key under src/ exists in every locale", () => {
  const files = sourceFiles(SRC_DIR);
  assert.ok(files.length > 0, "no source files scanned");
  assert.deepEqual(missingKeys(files), []);
});
