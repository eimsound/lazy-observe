import assert from "node:assert/strict";
import { test } from "vitest";

import { safeRedirect } from "./safe-redirect.ts";

const ORIGIN = "https://observe.example.com";

test("safeRedirect preserves valid same-origin paths with query and hash", () => {
  assert.equal(safeRedirect("/", { origin: ORIGIN }), "/");
  assert.equal(
    safeRedirect("/settings/users?source=e2e#top", { origin: ORIGIN }),
    "/settings/users?source=e2e#top",
  );
  assert.equal(
    safeRedirect("/logs?q=error&run=1", { origin: ORIGIN }),
    "/logs?q=error&run=1",
  );
});

test("safeRedirect blocks open redirects via protocol-relative, backslash, scheme, and control chars", () => {
  const malicious = [
    "//evil.example",
    "//evil.example/path",
    "/\\evil.example",
    "\\/evil.example",
    "\\\\evil.example",
    "/settings\\..\\evil.example",
    "https://evil.example/login",
    "http://observe.example.com/settings",
    "javascript:alert(1)",
    "/\t/evil.example",
    "/\n/evil.example",
    "/\r/evil.example",
    "settings/users",
    "",
    "   ",
    undefined,
    null,
  ];
  for (const target of malicious) {
    assert.equal(
      safeRedirect(target, { origin: ORIGIN }),
      "/",
      `expected ${JSON.stringify(target)} to fall back to /`,
    );
  }
});

test("safeRedirect blocks auth-route self-loops", () => {
  assert.equal(safeRedirect("/login", { origin: ORIGIN }), "/");
  assert.equal(safeRedirect("/login?redirect=/logs", { origin: ORIGIN }), "/");
  assert.equal(safeRedirect("/login/callback", { origin: ORIGIN }), "/");

  assert.equal(
    safeRedirect("/change-password", {
      origin: ORIGIN,
      disallowPrefixes: ["/login", "/change-password"],
    }),
    "/",
  );
  assert.equal(
    safeRedirect("/change-password?redirect=/metrics", {
      origin: ORIGIN,
      disallowPrefixes: ["/login", "/change-password"],
    }),
    "/",
  );
  assert.equal(
    safeRedirect("/change-password-help", {
      origin: ORIGIN,
      disallowPrefixes: ["/login", "/change-password"],
    }),
    "/change-password-help",
  );
});
