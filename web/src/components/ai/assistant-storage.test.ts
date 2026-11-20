import assert from "node:assert/strict";
import { test } from "vitest";

import {
  ASSISTANT_MAX_TURNS,
  assistantHistoryKey,
  clearAssistantHistory,
  loadAssistantHistory,
  saveAssistantHistory,
  type AssistantStorageLike,
  type AssistantTurn,
} from "./assistant-storage.ts";

function memoryStorage(): AssistantStorageLike & { entries(): Record<string, string> } {
  const store = new Map<string, string>();
  return {
    getItem: (key) => store.get(key) ?? null,
    setItem: (key, value) => void store.set(key, value),
    removeItem: (key) => void store.delete(key),
    entries: () => Object.fromEntries(store.entries()),
  };
}

function doneTurn(id: string, question: string): AssistantTurn {
  return {
    id,
    question,
    requestedSignal: "auto",
    status: "done",
    answer: {
      signal: "logs",
      query: "*",
      explanation: "ok",
    },
  };
}

test("assistant history is isolated per user and per project", () => {
  const storage = memoryStorage();
  const aliceAlpha = assistantHistoryKey({ id: 1, username: "alice" }, "alpha");
  const aliceBeta = assistantHistoryKey({ id: 1, username: "alice" }, "beta");
  const bobAlpha = assistantHistoryKey({ id: 2, username: "bob" }, "alpha");

  assert.ok(aliceAlpha && aliceBeta && bobAlpha);
  assert.notEqual(aliceAlpha, aliceBeta);
  assert.notEqual(aliceAlpha, bobAlpha);
  assert.equal(assistantHistoryKey(null, "alpha"), null);
  assert.equal(assistantHistoryKey({ username: "alice" }, ""), null);

  saveAssistantHistory(aliceAlpha, [doneTurn("1", "alpha secret")], storage);
  saveAssistantHistory(bobAlpha, [doneTurn("2", "bob secret")], storage);

  assert.deepEqual(
    loadAssistantHistory(aliceAlpha, storage).map((t) => t.question),
    ["alpha secret"],
  );
  assert.deepEqual(loadAssistantHistory(aliceBeta, storage), []);
  assert.deepEqual(
    loadAssistantHistory(bobAlpha, storage).map((t) => t.question),
    ["bob secret"],
  );

  clearAssistantHistory(aliceAlpha, storage);
  assert.deepEqual(loadAssistantHistory(aliceAlpha, storage), []);
  assert.deepEqual(
    loadAssistantHistory(bobAlpha, storage).map((t) => t.question),
    ["bob secret"],
  );
});

test("assistant history drops pending turns and caps persisted count", () => {
  const storage = memoryStorage();
  const key = assistantHistoryKey({ username: "alice" }, "alpha");
  const turns: AssistantTurn[] = [
    ...Array.from({ length: ASSISTANT_MAX_TURNS + 5 }, (_, i) => doneTurn(`t-${i}`, `q-${i}`)),
    { id: "pending", question: "in-flight", requestedSignal: "logs", status: "pending" },
  ];

  saveAssistantHistory(key, turns, storage);
  const loaded = loadAssistantHistory(key, storage);
  assert.equal(loaded.length, ASSISTANT_MAX_TURNS);
  assert.equal(loaded[0].id, "t-5");
  assert.equal(loaded.at(-1)?.id, `t-${ASSISTANT_MAX_TURNS + 4}`);
});
