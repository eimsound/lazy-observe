import assert from "node:assert/strict";
import { test } from "vitest";
import { QueryClient } from "@tanstack/react-query";

import { AUTH_QUERY_KEY, handleSessionExpiry } from "./session-expiry.ts";

function signedInClient(): QueryClient {
  const client = new QueryClient();
  client.setQueryData(AUTH_QUERY_KEY, { username: "previous-user" });
  client.setQueryData(["issues"], ["private issue"]);
  client.setQueryData(["projects"], [{ slug: "alpha" }]);
  return client;
}

test("a bare 401 signs the user out and drops every cached result", () => {
  const client = signedInClient();
  handleSessionExpiry(client, { status: 401 });
  // The authed layout redirects to /login once the user is null.
  assert.equal(client.getQueryData(AUTH_QUERY_KEY), null);
  assert.equal(client.getQueryData(["issues"]), undefined);
  assert.equal(client.getQueryData(["projects"]), undefined);
});

test("coded 401s are form errors, not an expired session", () => {
  const client = signedInClient();
  handleSessionExpiry(client, { status: 401, code: "invalid_current_password" });
  handleSessionExpiry(client, { status: 403 });
  handleSessionExpiry(client, new Error("network"));
  assert.deepEqual(client.getQueryData(AUTH_QUERY_KEY), { username: "previous-user" });
  assert.deepEqual(client.getQueryData(["issues"]), ["private issue"]);
});
