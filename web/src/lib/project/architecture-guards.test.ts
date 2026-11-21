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

const NON_DATA_ROUTE_PAGES = new Set([
  "login-page.tsx",
  "change-password-page.tsx",
  "settings-page.tsx",
]);

test("every data-rendering route page uses queryViewState so errors never fall through to EmptyState", () => {
  const routesDir = join(SRC_ROOT, "routes");
  const violations: string[] = [];
  for (const entry of readdirSync(routesDir)) {
    if (!entry.endsWith("-page.tsx") || NON_DATA_ROUTE_PAGES.has(entry)) continue;
    const content = stripComments(readFileSync(join(routesDir, entry), "utf8"));
    if (!/\bqueryViewState\s*\(/.test(content)) {
      violations.push(entry);
    }
  }
  assert.deepEqual(
    violations,
    [],
    `Every data route page must use queryViewState() for 5-state rendering: ${violations.join(", ")}`,
  );
});

test("sensitive and entity-edit dialogs are always conditionally mounted (never permanently mounted)", () => {
  const guardedDialogs: Array<{ file: string; component: string; requireKey: boolean }> = [
    { file: "routes/users-page.tsx", component: "CreateUserDialog", requireKey: false },
    { file: "routes/users-page.tsx", component: "ResetPasswordDialog", requireKey: true },
    { file: "routes/projects-page.tsx", component: "CreateProjectDialog", requireKey: false },
    { file: "routes/projects-page.tsx", component: "UpdateProjectDialog", requireKey: true },
    { file: "routes/projects-page.tsx", component: "TokensDialog", requireKey: true },
    { file: "routes/channels-page.tsx", component: "ChannelFormDialog", requireKey: true },
    { file: "routes/alert-rules-page.tsx", component: "RuleFormDialog", requireKey: true },
    { file: "routes/silences-page.tsx", component: "SilenceFormDialog", requireKey: true },
    { file: "routes/inhibition-rules-page.tsx", component: "InhibitionFormDialog", requireKey: true },
    { file: "routes/scrape-targets-page.tsx", component: "TargetFormDialog", requireKey: true },
  ];

  const violations: string[] = [];
  for (const { file, component, requireKey } of guardedDialogs) {
    const content = stripComments(readFileSync(join(SRC_ROOT, file), "utf8"));
    const mountRegex = new RegExp(`&&\\s*\\(?\\s*<${component}\\b([^>]*)>`, "s");
    const match = content.match(mountRegex);
    if (!match) {
      violations.push(`${file}: <${component}> must be conditionally mounted with &&`);
      continue;
    }
    if (requireKey && !/\bkey\s*=/.test(match[1])) {
      violations.push(`${file}: <${component}> must specify an entity key= prop`);
    }
  }
  assert.deepEqual(violations, [], violations.join("\n"));
});

test("form-backing queries disable window-focus refetches so dirty drafts cannot be clobbered", () => {
  const formFiles = [
    "routes/ai-settings-page.tsx",
    "components/settings/alert-config-card.tsx",
  ];
  for (const file of formFiles) {
    const content = stripComments(readFileSync(join(SRC_ROOT, file), "utf8"));
    assert.match(
      content,
      /refetchOnWindowFocus:\s*false/,
      `${file} must disable refetchOnWindowFocus on form-backing queries`,
    );
  }
});

test("authenticated layout enforces RouteAccessGuard and project-scoped outlet keying", () => {
  const content = stripComments(
    readFileSync(join(SRC_ROOT, "components/layout/authenticated-app-layout.tsx"), "utf8"),
  );
  assert.match(content, /<RouteAccessGuard>/);
  assert.match(content, /routeProjectScopeKey\(/);
});
