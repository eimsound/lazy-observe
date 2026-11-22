import assert from "node:assert/strict";
import { QueryClient } from "@tanstack/react-query";
import { test } from "vitest";
import {
  beginFieldValueRequest,
  createEmptyFieldMetadataCache,
  failFieldValueRequest,
  finishFieldValueRequest,
  succeedFieldValueRequest,
} from "./use-logs-field-metadata";

test("field metadata cache state transitions clear prior errors on retry and reset on identity change", () => {
  const initial = createEmptyFieldMetadataCache("id-1");
  let state = succeedFieldValueRequest(initial, "id-1", "env", [{ value: "prod", hits: 42 }]);

  state = beginFieldValueRequest(state, "id-1", "app");
  assert.equal(state.loading.app, true);
  assert.equal(state.errors.app, null);
  assert.deepEqual(state.values.env, [{ value: "prod", hits: 42 }]);

  state = failFieldValueRequest(state, "id-1", "app", "upstream 502");
  state = finishFieldValueRequest(state, "id-1", "app");
  assert.equal(state.loading.app, false);
  assert.equal(state.errors.app, "upstream 502");

  // Explicit retry on the same identity clears the error and enters loading state.
  state = beginFieldValueRequest(state, "id-1", "app");
  assert.equal(state.loading.app, true);
  assert.equal(state.errors.app, null);
  assert.deepEqual(state.values.env, [{ value: "prod", hits: 42 }]);

  state = succeedFieldValueRequest(state, "id-1", "app", [{ value: "api-gateway", hits: 10 }]);
  state = finishFieldValueRequest(state, "id-1", "app");
  assert.equal(state.loading.app, false);
  assert.equal(state.errors.app, null);
  assert.deepEqual(state.values.app, [{ value: "api-gateway", hits: 10 }]);

  // Changing identity discards stale values and errors from the prior identity.
  const next = beginFieldValueRequest(state, "id-2", "pod");
  assert.equal(next.identity, "id-2");
  assert.equal(next.loading.pod, true);
  assert.equal(next.errors.pod, null);
  assert.equal(next.values.app, undefined);
  assert.equal(next.values.env, undefined);
});

test("QueryClient.fetchQuery re-executes queryFn on retry after a rejected attempt", async () => {
  const client = new QueryClient();
  let attempts = 0;
  const queryKey = ["logs-explorer", "default", "stream-field-values", "app"] as const;

  const queryFn = async () => {
    attempts += 1;
    if (attempts === 1) {
      throw new Error("transient failure");
    }
    return { values: [{ value: "api-gateway", hits: 12 }] };
  };

  await assert.rejects(
    client.fetchQuery({ queryKey, staleTime: 30_000, retry: false, queryFn }),
    /transient failure/,
  );
  assert.equal(attempts, 1);

  const recovered = await client.fetchQuery({
    queryKey,
    staleTime: 30_000,
    retry: false,
    queryFn,
  });
  assert.equal(attempts, 2);
  assert.deepEqual(recovered.values, [{ value: "api-gateway", hits: 12 }]);
});
