import assert from "node:assert/strict";
import { test } from "vitest";
import dayjs from "../dayjs.ts";

import {
  normalizeFlameNode,
  normalizeProfileListResponse,
  normalizeProfileServicesResponse,
  type ProfileListResponseDTO,
} from "./profile-contract.ts";

test("normalizes the gateway profile envelope", () => {
  const response = JSON.parse(`{
    "profiles": [{
      "id": 42,
      "service": "checkout",
      "type": "cpu",
      "start": "2026-07-17T12:00:00Z",
      "end": "2026-07-17T12:00:10.25Z",
      "size": 8192,
      "trace_id": "0123456789abcdef0123456789abcdef"
    }]
  }`) as ProfileListResponseDTO;

  assert.deepEqual(normalizeProfileListResponse(response), [
    {
      id: "42",
      service: "checkout",
      type: "cpu",
      startMs: dayjs("2026-07-17T12:00:00Z").valueOf(),
      endMs: dayjs("2026-07-17T12:00:10.25Z").valueOf(),
      size: 8192,
      traceId: "0123456789abcdef0123456789abcdef",
    },
  ]);
});

test("unwraps services and rejects legacy or malformed response shapes", () => {
  assert.deepEqual(normalizeProfileServicesResponse({ services: ["api", "worker"] }), [
    "api",
    "worker",
  ]);
  assert.throws(
    () => normalizeProfileServicesResponse({ data: ["api"] } as never),
    /must contain services/,
  );
  assert.throws(
    () => normalizeProfileListResponse({ data: [] } as never),
    /must contain profiles/,
  );
});

test("rejects invalid IDs and timestamps at the API boundary", () => {
  const valid = {
    id: 1,
    service: "api",
    type: "cpu" as const,
    start: "2026-07-17T12:00:00Z",
    end: "2026-07-17T12:00:10Z",
    size: 10,
  };
  assert.throws(
    () => normalizeProfileListResponse({ profiles: [{ ...valid, id: 1.5 }] }),
    /positive integer/,
  );
  assert.throws(
    () => normalizeProfileListResponse({ profiles: [{ ...valid, start: "yesterday" }] }),
    /RFC3339/,
  );
});

test("normalizes Go flameNode tree with unit, nested children, and truncated flag", () => {
  // Exact wire shape from internal/gateway/profile_api.go flameNode:
  const tree = normalizeFlameNode({
    name: "root",
    value: 1500,
    unit: "nanoseconds",
    children: [
      {
        name: "main.handleCheckout",
        value: 1500,
        truncated: true,
        children: [{ name: "encoding/json.Marshal", value: 900 }],
      },
    ],
  });

  assert.equal(tree.name, "root");
  assert.equal(tree.value, 1500);
  assert.equal(tree.unit, "nanoseconds");
  assert.equal(tree.children?.[0]?.truncated, true);
  assert.equal(tree.children?.[0]?.children?.[0]?.value, 900);

  assert.throws(() => normalizeFlameNode({ name: "root", value: -1 }), /non-negative number/);
  assert.throws(() => normalizeFlameNode({ name: "root", value: 10, children: "bad" }), /children must be an array/);
});
