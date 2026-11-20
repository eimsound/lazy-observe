import assert from "node:assert/strict";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "vitest";

const SRC_ROOT = resolve(fileURLToPath(new URL("../..", import.meta.url)));

function walkSourceFiles(dir: string): string[] {
  const files: string[] = [];
  for (const entry of readdirSync(dir)) {
    const fullPath = join(dir, entry);
    const stat = statSync(fullPath);
    if (stat.isDirectory()) {
      files.push(...walkSourceFiles(fullPath));
    } else if (
      (entry.endsWith(".ts") || entry.endsWith(".tsx")) &&
      !entry.endsWith(".test.ts") &&
      !entry.endsWith(".test.tsx")
    ) {
      files.push(fullPath);
    }
  }
  return files;
}

function stripComments(source: string): string {
  return source
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/(^|[^:])\/\/.*$/gm, "$1");
}

const UI_DIRS = ["routes", "components", "hooks"].map((name) => join(SRC_ROOT, name));

test("UI layer never imports non-reactive getProjectSlug or setProjectSlug", () => {
  const violations: string[] = [];
  for (const dir of UI_DIRS) {
    for (const file of walkSourceFiles(dir)) {
      const content = stripComments(readFileSync(file, "utf8"));
      if (/\b(?:getProjectSlug|setProjectSlug)\b/.test(content)) {
        violations.push(relative(SRC_ROOT, file));
      }
    }
  }
  assert.deepEqual(
    violations,
    [],
    `UI files must use useProject() / useProjectQuery() instead of getProjectSlug/setProjectSlug: ${violations.join(", ")}`,
  );
});

test("UI layer never bypasses TanStack Router via window.history.replaceState/pushState or window.location.search", () => {
  const violations: string[] = [];
  for (const dir of UI_DIRS) {
    for (const file of walkSourceFiles(dir)) {
      const content = stripComments(readFileSync(file, "utf8"));
      if (
        /\bwindow\.history\.(?:replaceState|pushState)\b/.test(content) ||
        /\bwindow\.location\.search\b/.test(content)
      ) {
        violations.push(relative(SRC_ROOT, file));
      }
    }
  }
  assert.deepEqual(
    violations,
    [],
    `UI files must read/write URL search state through TanStack Router (useSearch/useRouterLocationSync/navigate): ${violations.join(", ")}`,
  );
});

test("UI layer never invokes raw fetch() directly, ensuring transport-layer 401 handling covers all calls", () => {
  const violations: string[] = [];
  for (const dir of UI_DIRS) {
    for (const file of walkSourceFiles(dir)) {
      const content = stripComments(readFileSync(file, "utf8"));
      if (/(?:^|[^\w.$])fetch\s*\(/m.test(content)) {
        violations.push(relative(SRC_ROOT, file));
      }
    }
  }
  assert.deepEqual(
    violations,
    [],
    `UI files must use web/src/lib/api/ transport helpers instead of raw fetch(): ${violations.join(", ")}`,
  );
});
