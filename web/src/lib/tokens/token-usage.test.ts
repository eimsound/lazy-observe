import assert from "node:assert/strict";
import { test } from "vitest";

import {
  grafanaDataSourceUrl,
  grafanaHeaderBlock,
  grafanaHeaders,
  mcpCommand,
  projectDsn,
  readProjectSlug,
} from "./token-usage.ts";

test("project DSN keeps the SDK shape <origin>/p/<slug>@<token>", () => {
  assert.equal(
    projectDsn("http://gateway:3100", "production", "lo_abc"),
    "http://gateway:3100/p/production@lo_abc",
  );
  assert.equal(
    projectDsn("http://gateway:3100/", "production", "lo_abc"),
    "http://gateway:3100/p/production@lo_abc",
  );
});

test("reads target the selected project and fall back to `default`", () => {
  assert.equal(readProjectSlug("staging"), "staging");
  assert.equal(readProjectSlug(""), "default");
  assert.equal(readProjectSlug("  "), "default");
  assert.equal(readProjectSlug(null), "default");
  assert.equal(readProjectSlug(undefined), "default");
});

test("MCP command carries a user-token DSN for the active project", () => {
  assert.equal(
    mcpCommand("http://localhost:3100", "", "lou_abc"),
    "lazy-observe mcp --dsn 'http://localhost:3100/p/default@lou_abc'",
  );
});

test("MCP command keeps a slug with a single quote inside one shell word", () => {
  const command = mcpCommand("http://localhost:3100", "te'am", "lou_abc");
  assert.equal(
    command,
    "lazy-observe mcp --dsn 'http://localhost:3100/p/te'\\''am@lou_abc'",
  );
  // Bash reads the escaped word back as exactly one DSN argument.
  const word = command.slice(command.indexOf("'"), command.lastIndexOf("'") + 1);
  assert.equal(word.slice(1, -1).replaceAll("'\\''", "'"), "http://localhost:3100/p/te'am@lou_abc");
});

test("Grafana data source is the gateway root with both custom headers", () => {
  assert.equal(grafanaDataSourceUrl("http://lazy-observe:3100/"), "http://lazy-observe:3100");
  assert.deepEqual(grafanaHeaders("lou_abc", "staging"), [
    { name: "Authorization", value: "Bearer lou_abc" },
    { name: "X-LO-Project", value: "staging" },
  ]);
});

test("the copyable Grafana header block carries the project selector", () => {
  assert.equal(
    grafanaHeaderBlock("lou_abc", ""),
    "Authorization: Bearer lou_abc\nX-LO-Project: default",
  );
});
